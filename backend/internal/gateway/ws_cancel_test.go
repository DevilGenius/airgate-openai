package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsTestPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	t.Cleanup(server.Close)
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	configureWebSocketConn(client)
	return client, peer
}

func TestReceiveWSResponseCancellationInterruptsRead(t *testing.T) {
	for _, control := range []int{0, websocket.PingMessage, websocket.PongMessage} {
		t.Run(strconv.Itoa(control), func(t *testing.T) {
			client, peer := wsTestPair(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan WSResult, 1)
			go func() { done <- ReceiveWSResponse(ctx, client, nil) }()
			if control != 0 {
				if err := peer.WriteControl(control, []byte("alive"), time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case result := <-done:
				if !errors.Is(result.Err, context.Canceled) {
					t.Fatalf("result error = %v", result.Err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not unblock WebSocket read")
			}
		})
	}
}

func TestWebSocketHeartbeatsDoNotExtendResponseDeadline(t *testing.T) {
	client, peer := wsTestPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if peer.WriteControl(websocket.PingMessage, []byte("alive"), time.Now().Add(time.Second)) != nil {
					return
				}
			}
		}
	}()
	started := time.Now()
	result := receiveWSResponse(ctx, client, nil, 50*time.Millisecond)
	cancel()
	<-stopped
	if result.Err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("heartbeat kept response alive: %v", result.Err)
	}
}
