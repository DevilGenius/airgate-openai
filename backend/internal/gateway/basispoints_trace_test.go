package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestBasispointsFinalErrorTraceAcrossProtocols(t *testing.T) {
	failed := `{"type":"response.failed","sequence_number":73,"response":{"id":"resp_trace","status":"failed","error":{"code":"server_error","message":"trace fixture"},"output":[{"type":"function_call","call_id":"native-call","name":"native_tool","arguments":"{}"}]}}`
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"http", "failed", "error", "non_sse", "malformed", "eof", "success", "disabled", "validation"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, scenario), func(t *testing.T) {
					req := bpsRequest()
					req.TraceFinalError = scenario != "disabled"
					req.Stream, req.Writer = stream, httptest.NewRecorder()
					req.Headers.Set("Content-Type", "application/json")
					req.DispatchPlan = sdk.DispatchPlan{WireModel: req.Model}
					switch protocol {
					case "chat", "anthropic":
						req.Body = []byte(`{"model":"gpt-5.6-sol","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
						path := "/v1/chat/completions"
						if protocol == "anthropic" {
							path = "/v1/messages"
						}
						req.Headers.Set("X-Airgate-Request-Path", path)
					}
					if scenario == "validation" {
						// Native continuation is rejected before the HTTP transport runs.
						req.Body = []byte(`{"model":"gpt-5.6-sol","input":"hi","previous_response_id":"resp_previous"}`)
						req.Headers.Set("X-Airgate-Request-Path", "/v1/responses")
					}
					var wire []byte
					calls := 0
					raw := failed
					status := http.StatusOK
					g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
						calls++
						wire, _ = io.ReadAll(r.Body)
						switch scenario {
						case "http":
							raw = `{"detail":[{"loc":["body","input"],"msg":"invalid fixture"}],"extra":"preserve original BAS validation"}`
							status = http.StatusUnprocessableEntity
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(status)
							_, _ = io.WriteString(w, raw)
							return
						case "non_sse":
							raw = "unexpected BAS response"
							w.Header().Set("Content-Type", "text/plain")
							_, _ = io.WriteString(w, raw)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						switch scenario {
						case "success":
							_, _ = io.WriteString(w, bpsCompleted)
						case "error":
							raw = `{"error":{"code":"server_error","message":"BAS error event"}}`
							_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", raw)
						case "malformed":
							raw = "{invalid-json"
							_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
						case "eof":
							raw = ""
						default:
							_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
						}
					})
					g.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
					outcome, err := g.Forward(context.Background(), req)
					if err != nil {
						t.Fatalf("Forward: %v", err)
					}
					if scenario == "validation" {
						if calls != 0 || outcome.Kind != sdk.OutcomeClientError || outcome.FinalErrorDiagnostic != nil {
							t.Fatalf("local validation invented upstream diagnostics: calls=%d outcome=%+v", calls, outcome)
						}
						return
					}
					if calls != 1 {
						t.Fatalf("upstream calls=%d", calls)
					}
					if scenario == "success" || scenario == "disabled" {
						if outcome.FinalErrorDiagnostic != nil {
							t.Fatal("diagnostics published for success or disabled trace")
						}
						if scenario == "success" && outcome.Kind != sdk.OutcomeSuccess {
							t.Fatalf("success=%+v", outcome)
						}
						return
					}
					if outcome.Kind == sdk.OutcomeSuccess {
						t.Fatal("failure reported as success")
					}
					diagnostic := outcome.FinalErrorDiagnostic
					if diagnostic == nil || len(diagnostic.OutboundRequests) != 1 {
						t.Fatalf("missing BAS trace: %+v", diagnostic)
					}
					if string(diagnostic.UpstreamErrorBody) != raw {
						t.Fatalf("raw event changed: got=%s want=%s", diagnostic.UpstreamErrorBody, raw)
					}
					outbound := diagnostic.OutboundRequests[0]
					if !bytes.Equal(outbound.Body, wire) || outbound.StatusCode != status || outbound.Transport != "http" || outbound.Method != http.MethodPost || !strings.HasSuffix(outbound.URL, "/basispoints/api/responses") {
						t.Fatalf("wire trace mismatch: %+v", outbound)
					}
					if gjson.GetBytes(outbound.Body, "service_tier").Exists() || gjson.GetBytes(outbound.Body, "model_selection").String() != "explicit" {
						t.Fatal("trace contains client body instead of BAS body")
					}
					if outbound.Headers.Get("Authorization") != "" || outbound.Headers.Get("X-OpenAI-Account-ID") != "" {
						t.Fatal("trace contains account credentials")
					}
				})
			}
		}
	}
}

func TestBasispointsTraceExcludesAttachmentRequests(t *testing.T) {
	imageBytes, encoded := bpsImageFixture(t)
	for _, uploadFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("upload_fails=%t", uploadFails), func(t *testing.T) {
			req := bpsRequest()
			req.TraceFinalError = true
			req.Body = []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, encoded))
			raw := `{"detail":"BAS attachment fixture failure"}`
			g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/attachments") && !uploadFails {
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `{"openai_file_id":"file-trace"}`)
					return
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, raw)
			})
			g.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			outcome, err := g.Forward(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := outcome.FinalErrorDiagnostic
			if uploadFails {
				if diagnostic != nil {
					t.Fatalf("attachment-only failure entered trace: %+v", diagnostic)
				}
				return
			}
			if diagnostic == nil || len(diagnostic.OutboundRequests) != 1 || string(diagnostic.UpstreamErrorBody) != raw {
				t.Fatalf("missing main request diagnostics: %+v", diagnostic)
			}
			for _, outbound := range diagnostic.OutboundRequests {
				if !outbound.BodyRedacted || outbound.BodyOriginalSize == 0 || bytes.Contains(outbound.Body, imageBytes) || bytes.Contains(outbound.Body, []byte(encoded)) {
					t.Fatalf("image trace was not redacted: %+v", outbound)
				}
				if outbound.Headers.Get("Authorization") != "" {
					t.Fatal("trace leaked authorization")
				}
			}
			if !strings.HasSuffix(diagnostic.OutboundRequests[0].URL, "/responses") {
				t.Fatal("non-model request entered trace")
			}
		})
	}
}
