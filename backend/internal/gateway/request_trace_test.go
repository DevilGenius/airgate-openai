package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/DevilGenius/airgate-sdk/sdkgo/requesttrace"
)

func TestNativeOAuthRequestTraceAtWebSocketBoundary(t *testing.T) {
	for _, handshakeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "response_failure", true: "handshake_failure"}[handshakeFailure], func(t *testing.T) {
			rawError := []byte(`{"error":{"message":"native oauth fixture"}}`)
			if !handshakeFailure {
				rawError = []byte(`{"type":"response.failed","response":{"id":"resp_trace","status":"failed","error":{"code":"server_error","message":"native oauth fixture"}}}`)
			}
			sent := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if handshakeFailure {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(401)
					_, _ = w.Write(rawError)
					return
				}
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				sent <- body
				_ = conn.WriteMessage(websocket.TextMessage, rawError)
			}))
			defer server.Close()
			ctx, capture := requesttrace.Start(t.Context(), true)
			conn, _, err := DialWebSocket(ctx, WSConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Token: "secret", AccountID: "private-account"})
			if handshakeFailure {
				if err == nil {
					_ = conn.Close()
					t.Fatal("expected failed handshake")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				body := map[string]any{"type": "response.create", "model": "gpt-test", "input": strings.Repeat("complete oauth history ", 4096)}
				raw, _ := json.Marshal(body)
				if err := writeWebSocketJSON(conn, json.RawMessage(raw)); err != nil {
					t.Fatal(err)
				}
				result := ReceiveWSResponse(ctx, conn, nil)
				if result.Err == nil {
					t.Fatal("expected failed response")
				}
			}
			outcome := sdk.ForwardOutcome{Kind: sdk.OutcomeClientError}
			capture.Finish(&outcome, nil)
			trace := outcome.FinalErrorDiagnostic
			if trace == nil || len(trace.OutboundRequests) != 1 || !bytes.Equal(trace.UpstreamErrorBody, rawError) {
				t.Fatalf("missing native OAuth trace: %+v", trace)
			}
			outbound := trace.OutboundRequests[0]
			if outbound.Headers.Get("Authorization") != "Bearer secret" || outbound.Headers.Get("ChatGPT-Account-ID") != "private-account" {
				t.Fatal("raw account headers were changed before Core preprocessing")
			}
			if !handshakeFailure && !bytes.Equal(outbound.Body, <-sent) {
				t.Fatal("trace did not preserve complete native wire request")
			}
		})
	}
}

func TestAPIKeyRequestTraceAtSharedHTTPBoundary(t *testing.T) {
	rawError := `{"error":{"message":"fixture validation failure"}}`
	var wire []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, rawError)
	}))
	defer server.Close()
	g := &OpenAIGateway{transportPool: NewTransportPool()}
	defer g.transportPool.CloseIdle()
	req := &sdk.ForwardRequest{TraceFinalError: true, Account: &sdk.Account{ID: 990, Credentials: map[string]string{"api_key": "test", "base_url": server.URL}}, Model: "gpt-test", Body: []byte(`{"model":"gpt-test","input":"full input","extension":{"keep":true}}`), Headers: http.Header{"Content-Type": {"application/json"}, "X-Airgate-Request-Path": {"/v1/responses"}}}
	outcome, err := g.Forward(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	trace := outcome.FinalErrorDiagnostic
	if trace == nil || len(trace.OutboundRequests) != 1 || !bytes.Equal(trace.OutboundRequests[0].Body, wire) || string(trace.UpstreamErrorBody) != rawError {
		t.Fatalf("HTTP trace missing: %+v", trace)
	}
}
