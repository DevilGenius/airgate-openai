package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRequestCancellationInterruptsWebSocketWriteBeforeReceive(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, _, err := DialWebSocket(ctx, WSConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	peer := <-accepted
	defer func() { _ = peer.Close() }()
	done := make(chan error, 1)
	go func() {
		data := make([]byte, 64<<10)
		for range 512 {
			if err := writeWebSocketMessage(conn, websocket.TextMessage, data); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		t.Fatalf("fixture did not block the socket: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write succeeded after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("request cancellation left socket write blocked")
	}
}
