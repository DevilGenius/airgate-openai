package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const compactV2TestBody = `{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":"Remember release 42"},{"type":"compaction_trigger"}]}`

func TestCompactionV2APIKeyStreamingRoundTrip(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses"} {
		t.Run(path, func(t *testing.T) {
			received := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" {
					t.Errorf("upstream path = %q", r.URL.Path)
				}
				if r.Header.Get(codexBetaFeaturesHeader) != "other_feature,remote_compaction_v2" {
					t.Errorf("beta features = %q", r.Header.Get(codexBetaFeaturesHeader))
				}
				body, _ := io.ReadAll(r.Body)
				received <- body
				w.Header().Set("Content-Type", "text/event-stream")
				item := `{"type":"compaction","id":"cmp_test","encrypted_content":"opaque-test-content"}`
				_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
				_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[%s],\"usage\":{\"input_tokens\":100,\"output_tokens\":10,\"total_tokens\":110}}}\n\n", item)
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			req := &sdk.ForwardRequest{
				Account: &sdk.Account{ID: 1, Credentials: map[string]string{"api_key": "test", "base_url": server.URL}},
				Body:    []byte(compactV2TestBody), Model: "gpt-5.6-sol", Stream: true, Writer: recorder,
				Headers: http.Header{"X-Forwarded-Path": {path}, codexBetaFeaturesHeader: {"other_feature"}},
			}
			gateway := &OpenAIGateway{transportPool: NewTransportPool()}
			defer gateway.transportPool.CloseIdle()
			outcome, err := gateway.forwardHTTP(context.Background(), req)
			if err != nil || outcome.Kind != sdk.OutcomeSuccess {
				t.Fatalf("forwardHTTP = %+v, %v", outcome, err)
			}
			select {
			case wire := <-received:
				if !gjson.GetBytes(wire, "stream").Bool() || gjson.GetBytes(wire, "input.1.type").String() != "compaction_trigger" {
					t.Fatalf("compaction wire changed: %s", wire)
				}
			default:
				t.Fatal("upstream was not called")
			}
			for _, want := range []string{"response.output_item.done", "response.completed", "opaque-test-content"} {
				if !strings.Contains(recorder.Body.String(), want) {
					t.Fatalf("missing %s in response: %s", want, recorder.Body.String())
				}
			}
			if outcome.Usage == nil || outcome.Usage.InputTokens != 100 || outcome.Usage.OutputTokens != 10 {
				t.Fatalf("compaction usage = %+v", outcome.Usage)
			}
		})
	}
}

func TestCompactionV2OAuthRequestAndReplay(t *testing.T) {
	gateway := &OpenAIGateway{}
	account := &sdk.Account{Credentials: map[string]string{"access_token": "test"}}
	for _, body := range []string{
		compactV2TestBody,
		`{"type":"response.create","model":"gpt-5.6-sol","input":[{"type":"compaction","encrypted_content":"opaque-test-content"},{"role":"user","content":"Continue"},{"type":"compaction_trigger"}]}`,
	} {
		req := &sdk.ForwardRequest{Account: account, Model: "gpt-5.6-sol", Body: []byte(body), Headers: http.Header{}}
		wire, err := gateway.buildWSRequest(req, openAISessionResolution{})
		if err != nil {
			t.Fatal(err)
		}
		input := gjson.GetBytes(wire, "input").Array()
		if len(input) < 2 || input[len(input)-1].Get("type").String() != "compaction_trigger" {
			t.Fatalf("trigger missing or displaced: %s", wire)
		}
		if gjson.GetBytes(wire, "type").String() != "response.create" || !gjson.GetBytes(wire, "stream").Bool() || gjson.GetBytes(wire, "store").Bool() {
			t.Fatalf("invalid WS request: %s", wire)
		}
		if strings.Contains(body, "opaque-test-content") && input[0].Get("encrypted_content").String() != "opaque-test-content" {
			t.Fatalf("compaction replay was lost: %s", wire)
		}
	}
	for _, feature := range []string{"", "other_feature", "remote_compaction_v2,other_feature,remote_compaction_v2"} {
		client := http.Header{codexBetaFeaturesHeader: {feature}}
		headers, err := gateway.buildOpenAIWebSocketHeaders(t.Context(), account, client, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		got := headers.Get(codexBetaFeaturesHeader)
		if strings.Count(got, remoteCompactionV2Feature) != 1 || (strings.Contains(feature, "other_feature") && !strings.Contains(got, "other_feature")) {
			t.Fatalf("features = %q for %q", got, feature)
		}
		if client.Get(codexBetaFeaturesHeader) != feature {
			t.Fatal("client headers mutated")
		}
	}
}

func TestCompactionV2RoutesOnly(t *testing.T) {
	for _, route := range PluginRouteDefinitions() {
		if strings.HasSuffix(route.Path, "/compact") {
			t.Fatalf("legacy route advertised: %+v", route)
		}
	}
	for _, rule := range openAIDispatchDSL().Rules {
		if rule.Model.StripSuffix != "" {
			t.Fatalf("legacy model suffix: %+v", rule)
		}
		for _, path := range rule.When.Paths {
			if strings.HasSuffix(path, "/compact") {
				t.Fatalf("legacy dispatch: %+v", rule)
			}
		}
	}
	req := &sdk.ForwardRequest{Body: []byte(compactV2TestBody)}
	if _, ok := textRequestHash(req, http.MethodPost, "/v1/responses"); ok {
		t.Fatal("compaction entered text request cache")
	}
	if _, ok := textPromptHash(req, http.MethodPost, "/v1/responses"); ok {
		t.Fatal("compaction entered text prompt cache")
	}
}

func TestCompactionV2OutputDetection(t *testing.T) {
	for _, event := range []string{"response.output_item.added", "response.output_item.done", "response.completed"} {
		for _, content := range []string{`"ciphertext"`, `""`, `null`, `123`} {
			item := `{"type":"compaction","encrypted_content":` + content + `}`
			data := `{"type":"` + event + `","item":` + item + `}`
			if event == "response.completed" {
				data = `{"type":"response.completed","response":{"output":[` + item + `]}}`
			}
			if got, want := streamDataHasOutput(data), content == `"ciphertext"`; got != want {
				t.Fatalf("output detection = %v, want %v for %s", got, want, data)
			}
		}
	}
	if streamDataHasOutput(`{"type":"response.compaction.compacting"}`) {
		t.Fatal("compaction progress is not completed output")
	}
}
