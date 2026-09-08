package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestReceiveWSResponseReturnsEncryptedContentErrorDetails(t *testing.T) {
	const errorObject = `{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified.","param":"input[1].encrypted_content"}`
	const errorBody = `{"error":` + errorObject + `}`
	for _, event := range []string{
		`{"type":"error","error":` + errorObject + `}`,
		`{"type":"response.failed","response":{"status":"failed","error":` + errorObject + `}}`,
	} {
		client, peer := wsTestPair(t)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := peer.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
			t.Fatal(err)
		}
		result := ReceiveWSResponse(ctx, client, nil)
		var failure *responsesFailureError
		if !errors.As(result.Err, &failure) {
			t.Fatalf("WebSocket error = %v, want structured failure", result.Err)
		}
		if failure.outcomeKind() != sdk.OutcomeClientError || failure.failoverScopeForKind(failure.outcomeKind()) != sdk.FailoverScopeNone {
			t.Fatalf("WebSocket verification failure permits failover: %+v", failure)
		}
		if canReplayContinuationAnchor(result.Err) || isPreviousResponseNotFoundError(result.Err) || isFunctionCallOutputWithoutCallErrorResult(result.Err) {
			t.Fatalf("WebSocket verification failure permits continuation recovery: %+v", failure)
		}
		if failure.StatusCode != http.StatusBadRequest || string(failure.openAIErrorBody(failure.codeOrKind())) != errorBody {
			t.Fatalf("WebSocket error details changed: %s", failure.openAIErrorBody(failure.codeOrKind()))
		}
		if string(result.FailedEventRaw) != event {
			t.Fatalf("WebSocket failed event changed: %s", result.FailedEventRaw)
		}
	}
}

func TestInvalidEncryptedContentCodePreventsOtherRecovery(t *testing.T) {
	for _, message := range []string{
		"The previous response was not found.",
		"No tool call found for function call output with call_id call_1.",
		"The requested model is not supported.",
		"Your input exceeds the context window of this model.",
	} {
		failure := classifyResponsesError("invalid_request_error", "invalid_encrypted_content", message)
		if failure.Kind != responsesFailureKindEncryptedContent || failure.outcomeKind() != sdk.OutcomeClientError {
			t.Fatalf("explicit verification code lost priority: %+v", failure)
		}
		body := openAIErrorJSON("invalid_request_error", "invalid_encrypted_content", message)
		outcome := failureOutcome(http.StatusBadRequest, body, nil, message, 0)
		if outcome.Kind != sdk.OutcomeClientError || outcome.FailoverScope != sdk.FailoverScopeNone || outcome.SafetyRejected {
			t.Fatalf("explicit verification code permits retry or account penalty: %+v", outcome)
		}
		if canReplayContinuationAnchor(failure) {
			t.Fatalf("explicit verification code permits continuation replay: %+v", failure)
		}
		if outcomeIsPreviousResponseNotFound(outcome) || outcomeIsFunctionCallOutputWithoutCall(outcome) || isContextWindowExceededForwardResult(outcome, failure) {
			t.Fatalf("HTTP verification error message permits recovery: %+v", outcome)
		}
	}
}
