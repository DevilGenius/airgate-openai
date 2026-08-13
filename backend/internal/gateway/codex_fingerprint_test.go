package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestResolveCodexFingerprintIDsModes(t *testing.T) {
	account := &sdk.Account{ID: 42, Credentials: map[string]string{}}
	if got := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintOff); got != nil {
		t.Fatal("off mode should not produce fingerprint IDs")
	}

	device := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintDevice)
	if device == nil || device.installationID == "" || device.sessionID != "" || device.threadID != "" {
		t.Fatalf("device mode IDs = %+v", device)
	}
	session := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintSession)
	if session == nil || session.installationID == "" || session.sessionID == "" || session.threadID == "" {
		t.Fatalf("session mode IDs = %+v", session)
	}
	if session.threadID == session.sessionID {
		t.Fatal("session mode should derive a separate thread from the client session")
	}
	full := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintFull)
	if full == nil || full.threadID != full.sessionID {
		t.Fatalf("full mode should converge thread to session: %+v", full)
	}
	if again := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintSession); again.installationID != session.installationID || again.sessionID != session.sessionID || again.threadID != session.threadID {
		t.Fatal("installation/session/thread IDs should be stable for the same account and client session")
	}
}

func TestApplyCodexFingerprintUpdatesHeadersAndClientMetadata(t *testing.T) {
	ids := resolveCodexFingerprintIDs(&sdk.Account{ID: 7}, "client-session", codexFingerprintSession)
	headers := http.Header{"X-Codex-Turn-Metadata": []string{`{"sandbox":"workspace"}`}}
	applyCodexFingerprintHeaders(headers, ids)
	if headers.Get("x-codex-installation-id") != ids.installationID || headers.Get("session_id") != ids.sessionID || headers.Get("thread-id") != ids.threadID {
		t.Fatalf("fingerprint headers = %#v", headers)
	}
	var turnMetadata map[string]any
	if err := json.Unmarshal([]byte(headers.Get("x-codex-turn-metadata")), &turnMetadata); err != nil {
		t.Fatalf("turn metadata is not JSON: %v", err)
	}
	if turnMetadata["installation_id"] != ids.installationID || turnMetadata["thread_id"] != ids.threadID {
		t.Fatalf("turn metadata = %#v", turnMetadata)
	}

	body := applyCodexFingerprintBody([]byte(`{"model":"gpt-5.4","client_metadata":{"existing":"keep"}}`), ids)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	metadata, _ := payload["client_metadata"].(map[string]any)
	if metadata["existing"] != "keep" || metadata["session_id"] != ids.sessionID || metadata["thread_id"] != ids.threadID {
		t.Fatalf("client metadata = %#v", metadata)
	}
}
