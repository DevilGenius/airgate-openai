package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const basispointsModelAccessMessage = "Model access has changed. Available models will update automatically. Try again, or contact your workspace admin if no model is available."

func TestBasispointsModelAccessChangedDuringAttachmentUpload(t *testing.T) {
	_, encoded := bpsImageFixture(t)
	req := bpsRequest()
	req.Body = []byte(fmt.Sprintf("{\"model\":\"gpt-5.6-sol\",\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,%s\"}]}]}", encoded))
	original := bytes.Clone(req.Body)
	var calls atomic.Int32
	g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/basispoints/api/attachments" {
			t.Error("generation called after rejected attachment")
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, basispointsModelAccessMessage)
	})
	outcome, used, err := g.tryBasispointsOAuth(t.Context(), req)
	if err != nil || used || outcome.Kind != sdk.OutcomeUnknown || calls.Load() != 1 || !bytes.Equal(req.Body, original) {
		t.Fatalf("attachment fallback failed: outcome=%+v used=%v err=%v calls=%d", outcome, used, err, calls.Load())
	}
}

func TestBasispointsModelAccessChangedStreamErrorDoesNotFallback(t *testing.T) {
	req := bpsRequest()
	req.Stream = true
	req.Writer = httptest.NewRecorder()
	var calls atomic.Int32
	g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"permission_denied\",\"message\":%q}}}\n\n", basispointsModelAccessMessage)
	})
	outcome, used, err := g.tryBasispointsOAuth(t.Context(), req)
	if err != nil || !used || outcome.Kind == sdk.OutcomeSuccess || calls.Load() != 1 {
		t.Fatalf("stream failure replayed: outcome=%+v used=%v err=%v calls=%d", outcome, used, err, calls.Load())
	}
}

func TestBasispointsModelAccessChangedDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"plain", 403, basispointsModelAccessMessage, true},
		{"nested message", 403, fmt.Sprintf("{\"error\":{\"message\":%q}}", basispointsModelAccessMessage), true},
		{"top-level message", 403, fmt.Sprintf("{\"message\":%q}", basispointsModelAccessMessage), true},
		{"detail", 403, fmt.Sprintf("{\"detail\":%q}", basispointsModelAccessMessage), true},
		{"case insensitive", 403, strings.ToUpper(basispointsModelAccessMessage), true},
		{"ordinary forbidden", 403, "forbidden", false},
		{"access denied code", 403, "{\"error\":{\"code\":\"model_access_denied\"}}", false},
		{"bad request", 400, basispointsModelAccessMessage, false},
		{"authentication", 401, basispointsModelAccessMessage, false},
		{"rate limited", 429, basispointsModelAccessMessage, false},
		{"server error", 503, basispointsModelAccessMessage, false},
		{"successful response", 200, basispointsModelAccessMessage, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBasispointsModelAccessChanged(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("fallback=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestBasispointsModelAccessChangedFallsThroughBeforeOutput(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				req := bpsRequest()
				req.Stream = stream
				if protocol == "chat" {
					req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
					req.Body = []byte("{\"model\":\"gpt-5.6-sol\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
				}
				before, headers, account := bytes.Clone(req.Body), req.Headers.Clone(), req.Account
				writer := &failoverProbeWriter{ResponseRecorder: httptest.NewRecorder()}
				req.Writer = writer
				var calls atomic.Int32
				g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, basispointsModelAccessMessage)
				})
				var outcome sdk.ForwardOutcome
				var used bool
				if protocol == "anthropic" {
					outcome, used = g.tryBasispointsAnthropic(t.Context(), req, req.Body, "claude-sonnet", req.Model, time.Now(), writer)
				} else {
					var err error
					outcome, used, err = g.tryBasispointsOAuth(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
				}
				if used || outcome.Kind != sdk.OutcomeUnknown || calls.Load() != 1 || writer.committed {
					t.Fatalf("used=%v kind=%s calls=%d committed=%v", used, outcome.Kind, calls.Load(), writer.committed)
				}
				if req.Account != account || !bytes.Equal(req.Body, before) || !reflect.DeepEqual(req.Headers, headers) {
					t.Fatal("fallback changed the account or original request")
				}
			})
		}
	}
}

func TestBasispointsModelAccessChangedRetriesSameAccountOAuth(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, nativeStatus := range []int{http.StatusOK, http.StatusForbidden} {
			t.Run(fmt.Sprintf("stream=%t/native=%d", stream, nativeStatus), func(t *testing.T) {
				req := bpsRequest()
				req.Stream = stream
				req.Headers.Set(standardKeyBillingHeader, "true")
				req.Body = []byte(fmt.Sprintf("{\"model\":\"claude-sonnet\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":1024,\"stream\":%t,\"service_tier\":\"priority\"}", stream))
				body, err := json.Marshal(map[string]any{"model": req.Model, "input": "hi", "stream": true, "service_tier": "priority"})
				if err != nil {
					t.Fatal(err)
				}
				before, bodyBefore := bytes.Clone(req.Body), bytes.Clone(body)
				nativeWire := serviceTierTestResponse(t, serviceTierTestProtocol{anthropic: true, stream: stream}, "priority", "response.completed", "")
				writer := &failoverProbeWriter{ResponseRecorder: httptest.NewRecorder()}
				req.Writer = writer
				var basCalls, nativeCalls atomic.Int32
				g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer test-token" {
						t.Error("fallback changed account credentials")
					}
					switch r.URL.Path {
					case "/basispoints/api/responses":
						basCalls.Add(1)
						if r.Header.Get("X-OpenAI-Account-ID") != "account-test" {
							t.Error("wrong BAS account")
						}
						w.WriteHeader(http.StatusForbidden)
						_, _ = fmt.Fprintf(w, "{\"error\":{\"message\":%q}}", basispointsModelAccessMessage)
					case "/backend-api/codex/responses":
						nativeCalls.Add(1)
						if r.Header.Get("ChatGPT-Account-ID") != "account-test" {
							t.Error("fallback changed OAuth account")
						}
						wire, _ := io.ReadAll(r.Body)
						if gjson.GetBytes(wire, "model").String() != req.Model || gjson.GetBytes(wire, "service_tier").String() != "priority" || gjson.GetBytes(wire, "model_selection").Exists() {
							t.Errorf("native fallback lost original parameters: %s", wire)
						}
						if nativeStatus != http.StatusOK {
							w.WriteHeader(nativeStatus)
							_, _ = fmt.Fprintf(w, "{\"error\":{\"message\":%q}}", basispointsModelAccessMessage)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, nativeWire)
					default:
						t.Errorf("unexpected upstream path %s", r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				})
				outcome, _, err := g.forwardAnthropicResponses(t.Context(), req, body, "claude-sonnet", req.Model, time.Now(), writer, true, openAISessionResolution{})
				if basCalls.Load() != 1 || nativeCalls.Load() != 1 {
					t.Fatalf("BAS calls=%d native calls=%d; expected one attempt per transport", basCalls.Load(), nativeCalls.Load())
				}
				if !bytes.Equal(before, req.Body) || !bytes.Equal(bodyBefore, body) || !basispointsEnabled(req) {
					t.Fatal("fallback mutated the request or disabled BAS permanently")
				}
				if nativeStatus == http.StatusForbidden {
					if outcome.Upstream.StatusCode != nativeStatus || writer.committed {
						t.Fatalf("native error lost: %+v committed=%v", outcome, writer.committed)
					}
					return
				}
				if err != nil || outcome.Kind != sdk.OutcomeSuccess || outcome.Usage == nil {
					t.Fatalf("native fallback failed: %+v err=%v", outcome, err)
				}
				payload := string(outcome.Upstream.Body)
				if stream {
					payload = writer.Body.String()
				}
				if !strings.Contains(payload, "OK") || strings.Contains(payload, basispointsModelAccessMessage) {
					t.Fatalf("bad client response: %s", payload)
				}
				applyProviderBillingPolicy(req, outcome.Usage)
				if usageMetadataText(outcome.Usage, "oauth_transport") == "basispoints" || usageMetadataText(outcome.Usage, "api_key.billing_policy") == "basispoints_standard" {
					t.Fatal("native fallback was billed as BAS")
				}
			})
		}
	}
}
