package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestAgentIdentityTaskRecoveryKeepsNewConcurrentTask(t *testing.T) {
	var registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := registrations.Add(1)
		_, _ = fmt.Fprintf(w, `{"task_id":"task-new-%d"}`, call)
	}))
	defer server.Close()

	previousBaseURL := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = server.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = previousBaseURL })

	account := newTestAgentIdentityAccount(t, "task-old")
	gateway := &OpenAIGateway{}
	oldHeaders, err := gateway.buildOpenAIAuthHeaders(context.Background(), account, false)
	if err != nil {
		t.Fatalf("build old auth headers: %v", err)
	}

	const workers = 2
	var wg sync.WaitGroup
	results := make(chan string, workers)
	errs := make(chan error, workers)
	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			gateway.invalidateAgentIdentityTask(account, oldHeaders)
			taskID, _, recoverErr := gateway.ensureAgentIdentityTask(context.Background(), account, true)
			if recoverErr != nil {
				errs <- recoverErr
				return
			}
			results <- taskID
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for recoverErr := range errs {
		t.Fatalf("recover task: %v", recoverErr)
	}
	for taskID := range results {
		if taskID != "task-new-1" {
			t.Fatalf("recovered task ID = %q, want task-new-1", taskID)
		}
	}
	if got := registrations.Load(); got != 1 {
		t.Fatalf("task registrations = %d, want 1", got)
	}

	// A late failure from a request that used task-old must not delete the task
	// another request has already recovered.
	gateway.invalidateAgentIdentityTask(account, oldHeaders)
	taskID, _, err := gateway.ensureAgentIdentityTask(context.Background(), account, true)
	if err != nil {
		t.Fatalf("reuse recovered task: %v", err)
	}
	if taskID != "task-new-1" || registrations.Load() != 1 {
		t.Fatalf("late invalidation recovered %q with %d registrations", taskID, registrations.Load())
	}

	// If the newly cached task itself is rejected, its assertion identifies the
	// exact cache entry and a new registration is performed.
	currentHeaders, err := gateway.buildOpenAIAuthHeaders(context.Background(), account, false)
	if err != nil {
		t.Fatalf("build current auth headers: %v", err)
	}
	gateway.invalidateAgentIdentityTask(account, currentHeaders)
	taskID, _, err = gateway.ensureAgentIdentityTask(context.Background(), account, true)
	if err != nil {
		t.Fatalf("recover current task: %v", err)
	}
	if taskID != "task-new-2" || registrations.Load() != 2 {
		t.Fatalf("current invalidation recovered %q with %d registrations", taskID, registrations.Load())
	}
}

func TestAgentIdentityAuthenticationErrorFinalization(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind sdk.OutcomeKind
	}{
		{name: "registration request", err: fmt.Errorf("%w: transport", errAgentIdentityTaskRegistrationRequestFailed), kind: sdk.OutcomeUpstreamTransient},
		{name: "rate limited", err: &agentIdentityTaskRegistrationHTTPError{StatusCode: http.StatusTooManyRequests}, kind: sdk.OutcomeUpstreamTransient},
		{name: "server error", err: &agentIdentityTaskRegistrationHTTPError{StatusCode: http.StatusBadGateway}, kind: sdk.OutcomeUpstreamTransient},
		{name: "context deadline", err: context.DeadlineExceeded, kind: sdk.OutcomeUpstreamTransient},
		{name: "invalid private key", err: errors.New("Agent Identity 私钥无效"), kind: sdk.OutcomeAccountDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := withAgentIdentityRequestState(context.Background())
			recordAgentIdentityAuthenticationError(ctx, tt.err)
			outcome := accountDeadOutcome("auth failed")
			mergeAgentIdentityUpdatedCredentials(&outcome, ctx)
			if outcome.Kind != tt.kind {
				t.Fatalf("outcome kind = %v, want %v", outcome.Kind, tt.kind)
			}
			if len(outcome.UpdatedCredentials) != 0 {
				t.Fatalf("internal authentication state leaked into credentials: %+v", outcome.UpdatedCredentials)
			}
		})
	}
}

func newTestAgentIdentityAccount(t *testing.T, taskID string) *sdk.Account {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Agent Identity key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal Agent Identity key: %v", err)
	}
	return &sdk.Account{
		ID: 42,
		Credentials: map[string]string{
			"auth_mode":         openAIAuthModeAgentIdentity,
			"agent_runtime_id":  "runtime-test",
			"agent_private_key": base64.StdEncoding.EncodeToString(der),
			"task_id":           taskID,
		},
	}
}
