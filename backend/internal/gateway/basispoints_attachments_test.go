package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func bpsImageFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestBasispointsImageUploadAcrossProtocols(t *testing.T) {
	imageBytes, encoded := bpsImageFixture(t)
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				req := bpsRequest()
				req.Stream, req.Writer = stream, httptest.NewRecorder()
				imagePart := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + encoded}
				inputKey := "input"
				if protocol == "chat" {
					inputKey = "messages"
					req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
					imagePart = map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + encoded}}
				}
				if protocol == "anthropic" {
					inputKey = "messages"
					imagePart = map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": encoded}}
				}
				req.Body, _ = json.Marshal(map[string]any{"model": req.Model, inputKey: []any{map[string]any{"role": "user", "content": []any{imagePart, imagePart}}}})
				uploads, calls := 0, 0
				g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-OpenAI-Account-ID") != "account-test" {
						t.Error("wrong selected account")
					}
					switch r.URL.Path {
					case "/basispoints/api/attachments":
						uploads++
						reader, err := r.MultipartReader()
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						part, err := reader.NextPart()
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						data, _ := io.ReadAll(part)
						if part.FormName() != "file" || part.Header.Get("Content-Type") != "image/png" || !bytes.Equal(data, imageBytes) {
							t.Error("bad multipart image")
						}
						w.WriteHeader(201)
						_, _ = io.WriteString(w, `{"openai_file_id":"file-uploaded"}`)
					case "/basispoints/api/responses":
						calls++
						wire, _ := io.ReadAll(r.Body)
						if uploads != 1 || bytes.Contains(wire, []byte("data:image")) || bytes.Contains(wire, []byte("file-airgate-inline-")) || bytes.Count(wire, []byte(`"file_id":"file-uploaded"`)) != 2 {
							t.Error("responses did not use uploaded images")
						}
						if gjson.GetBytes(wire, "service_tier").Exists() {
							t.Error("service tier leaked")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, bpsCompleted)
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						w.WriteHeader(404)
					}
				})
				var outcome sdk.ForwardOutcome
				var used bool
				if protocol == "anthropic" {
					body := convertAnthropicRequestToResponses(req.Body, req.Model, "low")
					outcome, used = g.tryBasispointsAnthropic(context.Background(), req, body, "claude-sonnet", req.Model, time.Now(), req.Writer)
				} else {
					var err error
					outcome, used, err = g.tryBasispointsOAuth(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
				}
				if !used || outcome.Kind != sdk.OutcomeSuccess || uploads != 1 || calls != 1 {
					t.Fatalf("used=%v outcome=%+v uploads=%d calls=%d", used, outcome, uploads, calls)
				}
			})
		}
	}
}

func TestBasispointsAttachmentErrorClassification(t *testing.T) {
	_, encoded := bpsImageFixture(t)
	for _, tc := range []struct {
		status  int
		message string
		kind    sdk.OutcomeKind
	}{
		{400, "image support is disabled for this account", sdk.OutcomeClientError},
		{400, "Organization disabled due to policy violation", sdk.OutcomeAccountDead},
		{401, "expired token", sdk.OutcomeAccountDead},
		{429, "rate limited", sdk.OutcomeAccountRateLimited},
		{503, "temporary failure", sdk.OutcomeUpstreamTransient},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.message), func(t *testing.T) {
			req := bpsRequest()
			req.Body = []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, encoded))
			g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/basispoints/api/attachments" {
					t.Error("generation called after failed upload")
				}
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": tc.message}})
			})
			outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
			if err != nil || !used || outcome.Kind != tc.kind || outcome.Upstream.StatusCode != tc.status {
				t.Fatalf("wrong upload classification: %+v used=%v err=%v", outcome, used, err)
			}
			if tc.kind == sdk.OutcomeClientError && outcome.ShouldFailover() {
				t.Error("image validation penalizes account")
			}
		})
	}
}

func TestBasispointsLocalValidationNeverDisablesAccount(t *testing.T) {
	// Historical error fixture only: AirGate has no image-disable configuration.
	// Keep this wording to catch the original disabled-feature/account mix-up.
	message := "basispoints does not accept data:image input while image support is disabled; provide an HTTPS image URL, or disable Basispoints"
	resp := basispointsInvalidRequestResponse(fmt.Errorf("%s", message))
	outcome := basispointsHTTPFailure(resp, time.Now())
	if outcome.Kind != sdk.OutcomeClientError || outcome.ShouldFailover() {
		t.Fatalf("local validation classified as account failure: %+v", outcome)
	}
	for _, message := range []string{"image support is disabled", "image support is disabled for this account", "uploads are suspended", "tools are deactivated"} {
		if got := classifyHTTPFailure(400, message); got != sdk.OutcomeClientError {
			t.Errorf("%s => %s", message, got)
		}
	}
	for _, message := range []string{"account suspended", "Your OpenAI account has been deactivated", "Organization disabled due to policy violation", "account_deactivated"} {
		if got := classifyHTTPFailure(400, message); got != sdk.OutcomeAccountDead {
			t.Errorf("real account failure lost: %s => %s", message, got)
		}
	}
	if !strings.Contains(string(outcome.Upstream.Body), "basispoints_request_invalid") {
		t.Error("validation code lost")
	}
}
