package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/google/uuid"
)

const (
	codexFingerprintModeConfigKey = "codex_fingerprint_mode"
)

type codexFingerprintMode string

const (
	codexFingerprintOff     codexFingerprintMode = "off"
	codexFingerprintDevice  codexFingerprintMode = "device"
	codexFingerprintSession codexFingerprintMode = "session"
	codexFingerprintFull    codexFingerprintMode = "full"
)

type codexFingerprintSettings struct {
	mode codexFingerprintMode
}

type codexFingerprintIDs struct {
	mode           codexFingerprintMode
	installationID string
	sessionID      string
	threadID       string
	turnID         string
	windowID       string
}

func (g *OpenAIGateway) codexFingerprintSettings() codexFingerprintSettings {
	settings := codexFingerprintSettings{mode: codexFingerprintOff}
	config := g.pluginConfig()
	if config == nil {
		return settings
	}

	switch strings.ToLower(strings.TrimSpace(config.GetString(codexFingerprintModeConfigKey))) {
	case string(codexFingerprintDevice):
		settings.mode = codexFingerprintDevice
	case string(codexFingerprintSession):
		settings.mode = codexFingerprintSession
	case string(codexFingerprintFull):
		settings.mode = codexFingerprintFull
	default:
		settings.mode = codexFingerprintOff
	}
	return settings
}

func deriveCodexFingerprintUUID(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16],
	)
}

func resolveCodexFingerprintIDs(account *sdk.Account, clientSessionID string, mode codexFingerprintMode) *codexFingerprintIDs {
	if account == nil || mode == codexFingerprintOff {
		return nil
	}

	installationID := deriveCodexFingerprintUUID(fmt.Sprintf("%s:codex-installation-id:v1:%d", PluginID, account.ID))
	if configured := strings.TrimSpace(account.Credentials["openai_device_id"]); configured != "" {
		installationID = configured
	}
	ids := &codexFingerprintIDs{
		mode:           mode,
		installationID: installationID,
	}
	if mode == codexFingerprintDevice {
		return ids
	}

	ids.sessionID = deriveCodexFingerprintUUID(fmt.Sprintf("%s:codex-session-id:v1:%d", PluginID, account.ID))
	if strings.TrimSpace(clientSessionID) != "" {
		ids.threadID = deriveCodexFingerprintUUID(fmt.Sprintf("%s:codex-thread-id:v1:%d:%s", PluginID, account.ID, clientSessionID))
	}
	if ids.threadID == "" {
		ids.threadID = ids.sessionID
	}
	if mode == codexFingerprintFull {
		ids.threadID = ids.sessionID
	}
	ids.turnID = uuid.NewString()
	ids.windowID = ids.threadID + ":0"
	return ids
}

func extractCodexClientSessionID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if value := strings.TrimSpace(headers.Get("session-id")); value != "" {
		return value
	}
	return strings.TrimSpace(headers.Get("session_id"))
}

func (g *OpenAIGateway) resolveCodexFingerprintIDs(account *sdk.Account, headers http.Header) *codexFingerprintIDs {
	settings := g.codexFingerprintSettings()
	if settings.mode == codexFingerprintOff {
		return nil
	}
	return resolveCodexFingerprintIDs(account, extractCodexClientSessionID(headers), settings.mode)
}

func applyCodexFingerprintHeaders(headers http.Header, ids *codexFingerprintIDs) {
	if headers == nil || ids == nil {
		return
	}

	headers.Set("x-codex-installation-id", ids.installationID)
	if ids.mode == codexFingerprintDevice {
		rewriteCodexTurnMetadata(headers, map[string]any{"installation_id": ids.installationID})
		return
	}

	headers.Set("x-codex-window-id", ids.windowID)
	headers.Set("x-client-request-id", ids.threadID)
	headers.Set("session-id", ids.sessionID)
	headers.Set("session_id", ids.sessionID)
	headers.Set("thread-id", ids.threadID)
	rewriteCodexTurnMetadata(headers, map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	})
}

func rewriteCodexTurnMetadata(headers http.Header, fields map[string]any) {
	raw := strings.TrimSpace(headers.Get("x-codex-turn-metadata"))
	if raw == "" {
		return
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return
	}
	for key, value := range fields {
		metadata[key] = value
	}
	if rebuilt, err := json.Marshal(metadata); err == nil {
		headers.Set("x-codex-turn-metadata", string(rebuilt))
	}
}

func applyCodexFingerprintBody(body []byte, ids *codexFingerprintIDs) []byte {
	if len(body) == 0 || ids == nil {
		return body
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	metadata, _ := payload["client_metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["x-codex-installation-id"] = ids.installationID
	if ids.mode != codexFingerprintDevice {
		metadata["session_id"] = ids.sessionID
		metadata["thread_id"] = ids.threadID
		metadata["turn_id"] = ids.turnID
		metadata["x-codex-window-id"] = ids.windowID
	}
	if raw, ok := metadata["x-codex-turn-metadata"].(string); ok && strings.TrimSpace(raw) != "" {
		var embedded map[string]any
		if json.Unmarshal([]byte(raw), &embedded) == nil {
			embedded["installation_id"] = ids.installationID
			if ids.mode != codexFingerprintDevice {
				embedded["session_id"] = ids.sessionID
				embedded["thread_id"] = ids.threadID
				embedded["turn_id"] = ids.turnID
				embedded["window_id"] = ids.windowID
				embedded["turn_started_at_unix_ms"] = time.Now().UnixMilli()
			}
			if rebuilt, err := json.Marshal(embedded); err == nil {
				metadata["x-codex-turn-metadata"] = string(rebuilt)
			}
		}
	}
	payload["client_metadata"] = metadata
	if rebuilt, err := json.Marshal(payload); err == nil {
		return rebuilt
	}
	return body
}

func applyCodexFingerprintWebSocketMessage(body []byte, ids *codexFingerprintIDs) []byte {
	if len(body) == 0 || ids == nil {
		return body
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	messageType, ok := payload["type"].(string)
	if !ok || strings.TrimSpace(messageType) != "response.create" {
		return body
	}
	return applyCodexFingerprintBody(body, ids)
}

func passCodexFingerprintCarrierHeaders(src, dst http.Header) {
	if src == nil || dst == nil {
		return
	}
	for _, key := range []string{
		"x-codex-installation-id", "x-codex-window-id", "x-client-request-id",
		"session-id", "session_id", "thread-id", "x-codex-turn-metadata",
	} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
}
