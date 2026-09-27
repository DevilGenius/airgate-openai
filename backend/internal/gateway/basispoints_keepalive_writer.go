package gateway

import (
	"io"
	"net/http"
)

// Private control event: upstream/native keepalive behavior remains v0.2.80's.
const basispointsKeepaliveEventType = "airgate.basispoints.keepalive"
const basispointsKeepaliveFrame = "event: airgate.basispoints.keepalive\ndata: {\"type\":\"airgate.basispoints.keepalive\"}\n\n"

type basispointsKeepaliveEncoder interface{ basispointsKeepalive() []byte }

func (*sseEventWriter) basispointsKeepalive() []byte {
	return []byte("event: keepalive\ndata: {\"type\":\"keepalive\"}\n\n")
}

func (s *chatCompletionsStreamWriter) basispointsKeepalive() []byte {
	frame := append([]byte("data: "), s.makeDeltaChunk(map[string]any{})...)
	return append(frame, '\n', '\n')
}

type anthropicBasispointsKeepalive struct{}

func (anthropicBasispointsKeepalive) basispointsKeepalive() []byte {
	return []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
}

// The normal response consumer is the only writer. A synthetic heartbeat cannot
// commit a retryable attempt, change timing/usage, or classify model progress.
func handleBasispointsKeepalive(kind string, w http.ResponseWriter, started bool, encoder basispointsKeepaliveEncoder) (bool, error) {
	if kind != basispointsKeepaliveEventType {
		return false, nil
	}
	if !started || w == nil {
		return true, nil
	}
	frame := encoder.basispointsKeepalive()
	n, err := w.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return true, err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return true, nil
}
