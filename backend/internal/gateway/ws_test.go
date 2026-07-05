package gateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

type trackingReadCloser struct {
	*strings.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestFormatWebSocketDialErrorClosesBody(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader(`{"error":{"message":"bad token"}}`)}
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       body,
	}

	err := formatWebSocketDialError(resp, errors.New("websocket: bad handshake"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !body.closed {
		t.Fatal("expected response body to be closed")
	}
}

func TestFormatWebSocketDialErrorTreatsDeactivatedWorkspaceAsAuthFailure(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       &trackingReadCloser{Reader: strings.NewReader(`{"detail":{"code":"deactivated_workspace"}}`)},
	}

	err := formatWebSocketDialError(resp, errors.New("websocket: bad handshake"))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "认证失败，access_token 已过期或账号已被停用") {
		t.Fatalf("error = %q, want auth failure hint", got)
	}
}
