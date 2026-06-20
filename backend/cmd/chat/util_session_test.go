package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type chatRoundTripFunc func(*http.Request) (*http.Response, error)

func (f chatRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func chatHTTPResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestMessageAndReasoningBuilders(t *testing.T) {
	user := buildUserMsg("hi")
	if user["type"] != "message" || user["role"] != "user" {
		t.Fatalf("user message = %#v", user)
	}
	userContent := user["content"].([]map[string]string)
	if userContent[0]["type"] != "input_text" || userContent[0]["text"] != "hi" {
		t.Fatalf("user content = %#v", userContent)
	}

	assistant := buildAssistantMsg("hello")
	if assistant["type"] != "message" || assistant["role"] != "assistant" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	assistantContent := assistant["content"].([]map[string]string)
	if assistantContent[0]["type"] != "output_text" || assistantContent[0]["text"] != "hello" {
		t.Fatalf("assistant content = %#v", assistantContent)
	}

	reasoning := buildReasoning("medium")
	if reasoning["effort"] != "medium" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
	}

	key := generateCacheKey()
	if !strings.HasPrefix(key, "chat-") || len(key) != len("chat-")+16 {
		t.Fatalf("cache key = %q", key)
	}
}

func TestBuildClient(t *testing.T) {
	client := buildClient("")
	if client.Timeout != 300*time.Second {
		t.Fatalf("timeout = %v", client.Timeout)
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("direct client should not set proxy")
	}

	client = buildClient("http://proxy.example:8080")
	transport = client.Transport.(*http.Transport)
	if transport.Proxy == nil {
		t.Fatal("proxy client should set proxy")
	}

	client = buildClient("://bad")
	transport = client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("invalid proxy should be ignored")
	}
}

func TestPrintStatsAndTerminalHandler(t *testing.T) {
	stderr := captureFileOutput(t, &os.Stderr, func() {
		printStats("gpt", 0, 0, 0, time.Second)
	})
	if stderr != "" {
		t.Fatalf("zero stats should not print, got %q", stderr)
	}
	stderr = captureFileOutput(t, &os.Stderr, func() {
		printStats("gpt", 1, 2, 3, 1234*time.Millisecond)
	})
	if !strings.Contains(stderr, "输入: 1") || !strings.Contains(stderr, "输出: 2") || !strings.Contains(stderr, "缓存: 3") {
		t.Fatalf("stats output = %q", stderr)
	}

	stdout := captureFileOutput(t, &os.Stdout, func() {
		(&terminalHandler{}).OnTextDelta("hello")
	})
	if stdout != "hello" {
		t.Fatalf("text delta output = %q", stdout)
	}
	stderr = captureFileOutput(t, &os.Stderr, func() {
		(&terminalHandler{}).OnReasoningDelta("thinking")
		(&terminalHandler{}).OnRateLimits(80)
		(&terminalHandler{}).OnRateLimits(80.1)
	})
	if !strings.Contains(stderr, "thinking") || !strings.Contains(stderr, "速率限制: 80.1%") {
		t.Fatalf("terminal stderr = %q", stderr)
	}
	(&terminalHandler{}).OnRawEvent("event", []byte(`{}`))
}

func TestSSESessionChatSuccessAndError(t *testing.T) {
	var sawRequest bool
	client := &http.Client{Transport: chatRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sawRequest = true
		if req.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("Authorization = %q", req.Header.Get("Authorization"))
		}
		if req.Header.Get("chatgpt-account-id") != "acct" {
			t.Fatalf("account header = %q", req.Header.Get("chatgpt-account-id"))
		}
		if req.Header.Get("x-codex-turn-state") != "turn-in" {
			t.Fatalf("turn state header = %q", req.Header.Get("x-codex-turn-state"))
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"prompt_cache_key":"cache"`) {
			t.Fatalf("request body missing cache key: %s", body)
		}
		stream := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`,
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}}`,
			`data: [DONE]`,
			``,
		}, "\n")
		return chatHTTPResponse(http.StatusOK, stream, http.Header{"X-Codex-Turn-State": []string{"turn-out"}}), nil
	})}
	session := &sseSession{
		client:    client,
		token:     "token",
		accountID: "acct",
		model:     "gpt-5.4",
		cacheKey:  "cache",
		turnState: "turn-in",
		reasoning: "low",
	}
	stdout := captureFileOutput(t, &os.Stdout, func() {
		_ = captureFileOutput(t, &os.Stderr, func() {
			if err := session.chat("hi"); err != nil {
				t.Fatalf("chat returned err: %v", err)
			}
		})
	})
	if !sawRequest {
		t.Fatal("SSE request was not sent")
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("stdout = %q", stdout)
	}
	if session.turnState != "turn-out" {
		t.Fatalf("turn state = %q", session.turnState)
	}
	if len(session.history) != 2 {
		t.Fatalf("history len = %d", len(session.history))
	}

	session.client = &http.Client{Transport: chatRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return chatHTTPResponse(http.StatusUnauthorized, "denied", nil), nil
	})}
	if err := session.chat("again"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("non-200 chat error = %v", err)
	}
}

func captureFileOutput(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	orig := *target
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	*target = writer
	fn()
	_ = writer.Close()
	*target = orig
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	_ = reader.Close()
	return string(out)
}
