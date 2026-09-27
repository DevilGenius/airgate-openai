package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

const bpsCompleted = `data: {"type":"response.completed","response":{"id":"resp_bps","model":"gpt-5.6-sol","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}}}

`

func bpsRequest() *sdk.ForwardRequest {
	return &sdk.ForwardRequest{Account: &sdk.Account{ID: 41, Credentials: map[string]string{"access_token": "test-token", "chatgpt_account_id": "account-test"}}, Model: "gpt-5.6-sol",
		Body:    []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hi"}]}`),
		Headers: http.Header{basispointsSettingHeader: {"true"}, "X-Airgate-Api-Key-Id": {"11"}}}
}

// Route the fixed production URL to a local TLS fixture; tests never contact BPS.
func bpsGateway(t *testing.T, req *sdk.ForwardRequest, serve http.HandlerFunc) *OpenAIGateway {
	t.Helper()
	server := httptest.NewTLSServer(serve)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	pool := NewTransportPool()
	pool.putLocked(poolKey(req.Account.ID, req.Account.ProxyURL), &transportPoolEntry{transport: transport, lastUsedAt: time.Now()})
	t.Cleanup(transport.CloseIdleConnections)
	return &OpenAIGateway{transportPool: pool}
}

func TestBasispointsToggleAndAccountEligibility(t *testing.T) {
	for _, test := range []struct {
		name, setting, apiKey, mode string
		want                        bool
	}{
		{name: "default off"}, {name: "off", setting: "false"}, {name: "on", setting: "true", want: true},
		{name: "api key", setting: "true", apiKey: "key"}, {name: "agent identity", setting: "true", mode: openAIAuthModeAgentIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := bpsRequest()
			req.Headers.Set(basispointsSettingHeader, test.setting)
			req.Account.Credentials["api_key"] = test.apiKey
			req.Account.Credentials["auth_mode"] = test.mode
			if got := basispointsEnabled(req); got != test.want {
				t.Fatalf("enabled=%v want %v", got, test.want)
			}
			if !test.want {
				outcome, used, err := (&OpenAIGateway{}).tryBasispointsOAuth(context.Background(), req)
				if used || err != nil || outcome.Kind != sdk.OutcomeUnknown {
					t.Fatalf("disabled attempted BPS: %+v %v %v", outcome, used, err)
				}
			}
		})
	}
}

func TestBasispointsResponsesAndChat(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "chat", false: "responses"}[chat], map[bool]string{true: "stream", false: "json"}[stream]}, "/"), func(t *testing.T) {
				req := bpsRequest()
				req.Stream = stream
				writer := httptest.NewRecorder()
				req.Writer = writer
				if chat {
					req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
					req.Body = []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`)
				}
				before := bytes.Clone(req.Body)
				var calls atomic.Int32
				g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/basispoints/api/responses" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-OpenAI-Account-ID") != "account-test" {
						t.Errorf("unexpected upstream request %s", r.URL)
					}
					if gjson.GetBytes(body, "model_selection").String() != "explicit" || gjson.GetBytes(body, "store").Bool() || !gjson.GetBytes(body, "stream").Bool() {
						t.Errorf("wire body %s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsCompleted)
				})
				outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
				if !used || err != nil || outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("outcome=%+v used=%v err=%v", outcome, used, err)
				}
				if calls.Load() != 1 || !bytes.Equal(before, req.Body) {
					t.Fatal("request replayed or mutated")
				}
				if outcome.Usage == nil || usageMetadataText(outcome.Usage, "oauth_transport") != "basispoints" {
					t.Fatalf("missing usage %+v", outcome.Usage)
				}
				payload := string(outcome.Upstream.Body)
				if stream {
					payload = writer.Body.String()
				}
				if !strings.Contains(payload, "hello") {
					t.Fatalf("missing output %s", payload)
				}
				if chat && !strings.Contains(payload, "choices") {
					t.Fatalf("not Chat protocol: %s", payload)
				}
			})
		}
	}
}

func TestBasispointsFallbackAndSharedFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		fallback bool
	}{
		{"unsupported model", 400, `{"error":{"code":"model_not_supported"}}`, true},
		{"no access", 403, `{"error":{"code":"model_access_denied"}}`, true},
		{"rate limited", 429, `{"error":{"code":"rate_limit_exceeded"}}`, false},
		{"forbidden", 403, `{"error":{"message":"forbidden"}}`, false},
		{"server failure", 503, `{"error":{"code":"internal_error"}}`, false},
		{"authentication", 401, `{"error":{"code":"invalid_token"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := bpsRequest()
			original := bytes.Clone(req.Body)
			var calls atomic.Int32
			g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
			if err != nil || used == tc.fallback || calls.Load() != 1 || !bytes.Equal(original, req.Body) {
				t.Fatalf("used=%v err=%v calls=%d", used, err, calls.Load())
			}
			if !tc.fallback {
				outcome = applyForwardOutcomePolicies(nil, req, outcome)
				want := failureOutcome(tc.status, []byte(tc.body), nil, extractOpenAIErrorMessage([]byte(tc.body)), 0)
				if outcome.Kind != want.Kind || !outcome.ShouldFailover() || outcome.Upstream.StatusCode != tc.status {
					t.Fatalf("BPS did not reuse native retry classification: %+v; want %s", outcome, want.Kind)
				}
			}
		})
	}
}

func TestBasispointsStreamInterruptedDoesNotReplay(t *testing.T) {
	req := bpsRequest()
	req.Stream = true
	req.Writer = httptest.NewRecorder()
	var calls atomic.Int32
	g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\",\"output_index\":0,\"content_index\":0}\n\n")
	})
	outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
	if !used || err != nil || outcome.Kind != sdk.OutcomeStreamAborted || calls.Load() != 1 {
		t.Fatalf("outcome=%+v used=%v err=%v calls=%d", outcome, used, err, calls.Load())
	}
	output := req.Writer.(*httptest.ResponseRecorder).Body.String()
	if strings.Contains(output, "[DONE]") || !strings.Contains(output, "response.failed") {
		t.Fatalf("bad stream termination %s", output)
	}
}

func TestBasispointsAnthropic(t *testing.T) {
	for _, stream := range []bool{false, true} {
		req := bpsRequest()
		body := bytes.Clone(req.Body)
		req.Stream = stream
		req.Body = []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`)
		writer := httptest.NewRecorder()
		g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, bpsCompleted)
		})
		outcome, used := g.tryBasispointsAnthropic(context.Background(), req, body, "claude-sonnet", req.Model, time.Now(), writer)
		if !used || outcome.Kind != sdk.OutcomeSuccess {
			t.Fatalf("outcome=%+v used=%v", outcome, used)
		}
		output := string(outcome.Upstream.Body)
		if stream {
			output = writer.Body.String()
		}
		if !strings.Contains(output, "hello") || strings.Contains(output, "run_officejs") {
			t.Fatalf("invalid Anthropic output %s", output)
		}
		if stream && (!strings.Contains(output, "event: message_start") || !strings.Contains(output, "event: message_stop")) {
			t.Fatalf("incomplete Anthropic lifecycle: %s", output)
		}
	}
}

func TestBasispointsIdentityIsolation(t *testing.T) {
	req := bpsRequest()
	baseline := basispointsIdentity(req, req.Body)
	req.Headers.Set("X-Airgate-API-Key-ID", "12")
	if baseline == basispointsIdentity(req, req.Body) {
		t.Fatal("API key scopes collide")
	}
	req = bpsRequest()
	req.Account.ID++
	if baseline == basispointsIdentity(req, req.Body) {
		t.Fatal("account scopes collide")
	}
	req = bpsRequest()
	req.Headers.Set("session_id", "other")
	if baseline == basispointsIdentity(req, req.Body) {
		t.Fatal("session scopes collide")
	}
}

func TestBasispointsToolsAreValidatedBeforeDelivery(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "invalid"}[valid], func(t *testing.T) {
			req := bpsRequest()
			req.Stream = true
			writer := httptest.NewRecorder()
			req.Writer = writer
			req.Body = []byte(`{"model":"gpt-5.6-sol","input":"run pwd","tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}}]}`)
			code := `{"name":"shell","arguments":{"cmd":"pwd"}}`
			if !valid {
				code = `{"name":"shell","arguments":{"wrong":true}}`
			}
			outer, _ := json.Marshal(map[string]any{"code": code, "summary": "Run shell", "references": []any{}, "destructive": false})
			item := map[string]any{"type": "function_call", "id": "fc_native", "call_id": "call_native", "name": "run_officejs", "arguments": string(outer), "status": "completed"}
			completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_tool", "model": req.Model, "status": "completed", "output": []any{item}}})
			g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if gjson.GetBytes(raw, "tools").Exists() {
					t.Error("client tools leaked upstream")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+string(completed)+"\n\n")
			})
			outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
			if !used || err != nil {
				t.Fatalf("used=%v err=%v", used, err)
			}
			if valid {
				if outcome.Kind != sdk.OutcomeSuccess || !strings.Contains(writer.Body.String(), `"name":"shell"`) || strings.Contains(writer.Body.String(), "run_officejs") {
					t.Fatalf("invalid tool result %+v %s", outcome, writer.Body.String())
				}
			} else if outcome.Kind == sdk.OutcomeSuccess || strings.Contains(writer.Body.String(), "function_call_arguments.delta") {
				t.Fatalf("invalid tool was delivered: %+v %s", outcome, writer.Body.String())
			}
		})
	}
}
