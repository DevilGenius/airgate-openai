package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestWebReverseImagesErrorClientStatusReturnsNilErr(t *testing.T) {
	outcome, err := webReverseImagesError(time.Now(), http.StatusBadRequest, nil, "图片尺寸不合法")
	if err != nil {
		t.Fatalf("expected nil err for client status, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d", outcome.Upstream.StatusCode, http.StatusBadRequest)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "图片尺寸不合法") {
		t.Fatalf("body = %s, want message to be preserved", outcome.Upstream.Body)
	}
}

func TestWebReverseImagesErrorAccountStatusKeepsErr(t *testing.T) {
	outcome, err := webReverseImagesError(time.Now(), http.StatusUnauthorized, nil, "OAuth 账号缺少 access_token")
	if err == nil {
		t.Fatalf("expected err for account status")
	}
	if outcome.Kind != sdk.OutcomeAccountDead {
		t.Fatalf("Kind = %v, want OutcomeAccountDead", outcome.Kind)
	}
}

func TestWebReverseAuthErrorClassification(t *testing.T) {
	transient, err := webReverseAuthError(time.Now(), context.DeadlineExceeded)
	if err == nil {
		t.Fatal("expected transient auth error")
	}
	if transient.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("transient Kind = %v, want OutcomeUpstreamTransient", transient.Kind)
	}
	if !strings.Contains(transient.Reason, "认证头失败") {
		t.Fatalf("transient reason = %q", transient.Reason)
	}

	dead, err := webReverseAuthError(time.Now(), errors.New("agent identity 私钥无效"))
	if err == nil {
		t.Fatal("expected account auth error")
	}
	if dead.Kind != sdk.OutcomeAccountDead {
		t.Fatalf("dead Kind = %v, want OutcomeAccountDead", dead.Kind)
	}
}

func TestTransientWebReverseAuthErrorUsesRegistrationTypes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "request failed", err: fmt.Errorf("%w: transport", errAgentIdentityTaskRegistrationRequestFailed), want: true},
		{name: "invalid response", err: errAgentIdentityTaskRegistrationResponseInvalid, want: true},
		{name: "missing task id", err: errAgentIdentityTaskRegistrationTaskIDMissing, want: true},
		{name: "rate limited", err: &agentIdentityTaskRegistrationHTTPError{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "server error", err: &agentIdentityTaskRegistrationHTTPError{StatusCode: http.StatusBadGateway}, want: true},
		{name: "unauthorized", err: &agentIdentityTaskRegistrationHTTPError{StatusCode: http.StatusUnauthorized}, want: false},
		{name: "credential error", err: errors.New("agent identity 私钥无效"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientWebReverseAuthError(tt.err); got != tt.want {
				t.Fatalf("isTransientWebReverseAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
