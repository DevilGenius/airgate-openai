package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type sseWriteRecorder struct {
	*httptest.ResponseRecorder
	writes   []string
	flushes  int
	failAt   int
	writeErr error
}

func (w *sseWriteRecorder) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	if len(w.writes) == w.failAt {
		if w.writeErr != nil {
			return 0, w.writeErr
		}
		return len(p) - 1, nil
	}
	return w.ResponseRecorder.Write(p)
}
func (w *sseWriteRecorder) Flush() { w.flushes++; w.ResponseRecorder.Flush() }

func TestSharedSSEWriterPreservesWriteAndFlushContract(t *testing.T) {
	for _, tc := range []struct {
		name  string
		send  func(http.ResponseWriter) error
		parts []string
	}{
		{"image_ping", writeSSEPing, []string{"event: ping\ndata: {}\n\n"}},
		{"done", writeSSEDone, []string{"data: [DONE]\n\n"}},
		{"data", func(w http.ResponseWriter) error { return writeSSEData(w, []byte("{}")) }, []string{"data: ", "{}", "\n\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &sseWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
			if err := tc.send(w); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(w.writes, tc.parts) || w.flushes != 1 {
				t.Fatalf("write boundaries or flush changed: %q flush=%d", w.writes, w.flushes)
			}
			for failAt := 1; failAt <= len(tc.parts); failAt++ {
				for _, cause := range []error{nil, errors.New("disconnected")} {
					w := &sseWriteRecorder{ResponseRecorder: httptest.NewRecorder(), failAt: failAt, writeErr: cause}
					want := cause
					if want == nil {
						want = io.ErrShortWrite
					}
					if err := tc.send(w); !errors.Is(err, want) || w.flushes != 0 || !reflect.DeepEqual(w.writes, tc.parts[:failAt]) {
						t.Fatalf("failure contract changed: %v writes=%q flush=%d", err, w.writes, w.flushes)
					}
				}
			}
		})
	}
}

func TestBPSSharedKeepalivePreservesProtocolAndCommitPolicy(t *testing.T) {
	chat := newChatCompletionsStreamWriter(nil, "gpt-test", 0, "", false, time.Now())
	chat.id, chat.created = "resp_stable", 123
	for _, tc := range []struct {
		name    string
		encoder basispointsKeepaliveEncoder
		want    string
	}{
		{"responses", &sseEventWriter{}, "event: keepalive\ndata: {\"type\":\"keepalive\"}\n\n"},
		{"anthropic", anthropicBasispointsKeepalive{}, "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{"chat", chat, "data: " + string(chat.makeDeltaChunk(map[string]any{})) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &sseWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
			if handled, err := handleBasispointsKeepalive(basispointsKeepaliveEventType, w, false, tc.encoder); !handled || err != nil || len(w.writes) != 0 || w.flushes != 0 {
				t.Fatal("heartbeat committed an untouched stream")
			}
			if handled, err := handleBasispointsKeepalive("keepalive", w, true, tc.encoder); handled || err != nil || len(w.writes) != 0 {
				t.Fatal("native event routing changed")
			}
			if handled, err := handleBasispointsKeepalive(basispointsKeepaliveEventType, w, true, tc.encoder); !handled || err != nil || w.Body.String() != tc.want || len(w.writes) != 1 || w.flushes != 1 {
				t.Fatalf("protocol frame or flush changed: %q %v", w.Body.String(), err)
			}
			for _, cause := range []error{nil, errors.New("disconnected")} {
				broken := &sseWriteRecorder{ResponseRecorder: httptest.NewRecorder(), failAt: 1, writeErr: cause}
				want := cause
				if want == nil {
					want = io.ErrShortWrite
				}
				if handled, err := handleBasispointsKeepalive(basispointsKeepaliveEventType, broken, true, tc.encoder); !handled || !errors.Is(err, want) || broken.flushes != 0 {
					t.Fatal("heartbeat write failure changed")
				}
			}
		})
	}
	if chat.id != "resp_stable" || chat.created != 123 || chat.wrote || chat.sentRole || chat.finalized {
		t.Fatal("heartbeat changed chat stream state")
	}
}
