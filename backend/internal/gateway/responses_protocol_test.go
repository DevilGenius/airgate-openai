package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestResponsesToolControlsSurviveNormalization(t *testing.T) {
	for _, fixture := range []struct{ name, fields string }{
		{"top_level", `"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"input":[]`},
		{"additional_tools", `"input":[{"type":"additional_tools","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]`},
		{"search_output", `"previous_response_id":"resp_1","tools":[],"input":[{"type":"tool_search_output","execution":"client","call_id":"search_1","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]`},
		{"continuation", `"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]`},
		{"filtered_top_level", `"tools":[{"type":"function","name":""}],"input":[{"type":"additional_tools","tools":[{"type":"function","name":"read"}]}]`},
		{"chat", `"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","parallel_tool_calls":false,"tool_choice":"none",` + fixture.fields + `}`)
			got, err := buildResponseCreateWSRequestWithHeaders(body, "gpt-5.6-sol", openAISessionResolution{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if parallel := gjson.GetBytes(got, "parallel_tool_calls"); parallel.Type != gjson.False {
				t.Fatalf("explicit false lost: %s", got)
			}
			if gjson.GetBytes(got, "tool_choice").String() != "none" {
				t.Fatalf("tool_choice lost: %s", got)
			}
			if gjson.GetBytes(got, codexResponsesLiteMetadataPath).Exists() {
				t.Fatalf("ordinary request activated Lite: %s", got)
			}
		})
	}
}

func TestResponsesExplicitModeIsModelIndependent(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-6-astra", "future-model"} {
		for _, marker := range []string{`true`, `" TrUe "`, `false`, `"false"`} {
			t.Run(model+"/"+marker, func(t *testing.T) {
				body := []byte(`{"type":"response.create","model":"` + model + `","parallel_tool_calls":true,"client_metadata":{"other":"keep","ws_request_header_x_openai_internal_codex_responses_lite":` + marker + `},"input":[{"type":"function_call","call_id":"call_1","namespace":"one","name":"read","arguments":"{}"},{"type":"function_call","call_id":"call_2","namespace":"two","name":"read","arguments":"{}"}]}`)
				got := sanitizeResponsesWebSocketClientMessage(body, responsesNormalizeOptions{strictCodex: true})
				lite := marker == `true` || marker == `" TrUe "`
				if gjson.GetBytes(got, "parallel_tool_calls").Bool() == lite {
					t.Fatalf("wrong mode: %s", got)
				}
				if gjson.GetBytes(got, "input.0.namespace").String() != "one" || gjson.GetBytes(got, "input.1.namespace").String() != "two" {
					t.Fatalf("namespaces lost: %s", got)
				}
				if gjson.GetBytes(got, "model").String() != model || gjson.GetBytes(got, "client_metadata.other").String() != "keep" || !gjson.GetBytes(got, codexResponsesLiteMetadataPath).Exists() {
					t.Fatalf("client metadata/model lost: %s", got)
				}
			})
		}
	}
}

func TestWebSocketProtocolHeaderSurvivesCredentialRefresh(t *testing.T) {
	for _, marker := range []string{"", "false", "true"} {
		t.Run(marker, func(t *testing.T) {
			captured := make(chan http.Header, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err == nil {
					_ = conn.Close()
				}
			}))
			defer server.Close()
			protocol := make(http.Header)
			if marker != "" {
				protocol.Set(codexResponsesLiteHeader, marker)
			}
			protocol.Set("Authorization", "must-not-forward")
			cfg := WSConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http")}
			gateway := &OpenAIGateway{}
			for _, credential := range []string{"Bearer initial", "Bearer refreshed"} {
				account := &sdk.Account{Credentials: map[string]string{"access_token": strings.TrimPrefix(credential, "Bearer ")}}
				headers, err := gateway.buildOpenAIWebSocketHeaders(t.Context(), account, protocol, nil, credential == "Bearer refreshed")
				if err != nil {
					t.Fatal(err)
				}
				cfg.Headers = headers
				conn, _, err := DialWebSocket(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
				headers = <-captured
				if headers.Get(codexResponsesLiteHeader) != marker || headers.Get("Authorization") != credential {
					t.Fatalf("dial dropped protocol or replaced credentials: %v", headers)
				}
			}
		})
	}
}

func TestBuildWebSocketHeadersRebuildsOwnedFields(t *testing.T) {
	gateway := &OpenAIGateway{}
	client := http.Header{
		codexResponsesLiteHeader: {"true"},
		"Authorization":          {"client-secret"},
		"Cookie":                 {"client-cookie"},
		"X-Codex-Turn-Metadata":  {"turn-metadata"},
	}
	ids := &codexFingerprintIDs{installationID: "installation"}
	for _, credentialType := range []string{"api_key", "access_token"} {
		t.Run(credentialType, func(t *testing.T) {
			account := &sdk.Account{Credentials: map[string]string{credentialType: "initial"}}
			first, err := gateway.buildOpenAIWebSocketHeaders(t.Context(), account, client, ids, false)
			if err != nil {
				t.Fatal(err)
			}
			first.Set("X-Stale-Auth", "stale")
			account.Credentials[credentialType] = "refreshed"
			second, err := gateway.buildOpenAIWebSocketHeaders(t.Context(), account, client, ids, true)
			if err != nil {
				t.Fatal(err)
			}
			if second.Get("Authorization") != "Bearer refreshed" || second.Get("X-Stale-Auth") != "" || second.Get("Cookie") != "" {
				t.Fatalf("rebuilt headers contain stale or client credentials: %v", second)
			}
			for _, key := range []string{codexResponsesLiteHeader, "X-Codex-Turn-Metadata", "X-Codex-Installation-Id"} {
				if first.Get(key) == "" || first.Get(key) != second.Get(key) {
					t.Fatalf("refresh lost %s", key)
				}
			}
			second.Set(codexResponsesLiteHeader, "false")
			if client.Get(codexResponsesLiteHeader) != "true" || first.Get(codexResponsesLiteHeader) != "true" {
				t.Fatal("builder aliased source or previous headers")
			}
		})
	}
}

type responsesProtocolClient struct {
	*websocket.Conn
	info sdk.WebSocketConnectInfo
}

func (c *responsesProtocolClient) ConnectInfo() *sdk.WebSocketConnectInfo { return &c.info }
func (c *responsesProtocolClient) Close(_ int, _ string) error            { return c.Conn.Close() }

func TestWebSocketBridgeUsesHandshakeLiteMarkerForEveryTurn(t *testing.T) {
	for _, marker := range []string{"", "false", "true"} {
		t.Run(marker, func(t *testing.T) {
			client, downstream := wsTestPair(t)
			upstream, peer := wsTestPair(t)
			headers := make(http.Header)
			if marker != "" {
				headers.Set(codexResponsesLiteHeader, marker)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- bridgeWebSocket(ctx, &responsesProtocolClient{Conn: client, info: sdk.WebSocketConnectInfo{Headers: headers}}, upstream, nil, true)
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("bridge did not stop")
				}
			}()
			for turn := 0; turn < 2; turn++ {
				body := []byte(`{"type":"response.create","model":"future-model","parallel_tool_calls":true,"input":[{"type":"function_call","call_id":"call_1","namespace":"workspace","name":"read","arguments":"{}"}]}`)
				if err := downstream.WriteMessage(websocket.TextMessage, body); err != nil {
					t.Fatal(err)
				}
				_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, got, err := peer.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if lite := gjson.GetBytes(got, codexResponsesLiteMetadataPath).String() == "true"; lite != (marker == "true") {
					t.Fatalf("turn %d lost handshake mode: %s", turn, got)
				}
				if gjson.GetBytes(got, "input.0.namespace").String() != "workspace" {
					t.Fatalf("namespace lost: %s", got)
				}
			}
		})
	}
}
