package imgen

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type closeTrackingTransport struct {
	closed bool
}

func (t *closeTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return imgenHTTPResponse(http.StatusOK, "", nil), nil
}

func (t *closeTrackingTransport) CloseIdleConnections() {
	t.closed = true
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func newTestClient(fn roundTripFunc) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		http:        &http.Client{Transport: fn, Jar: jar},
		accessToken: "access-token",
		deviceID:    "device-id",
		sessionID:   "session-id",
	}
}

func imgenHTTPResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestNewClientCloseAndRequestHelpers(t *testing.T) {
	proxy, _ := url.Parse("http://proxy.example:8080")
	c := NewClient("token", proxy)
	if c == nil || c.http == nil || c.accessToken != "token" || c.deviceID == "" || c.sessionID == "" {
		t.Fatalf("NewClient returned incomplete client: %#v", c)
	}
	if c.http.Jar == nil {
		t.Fatal("NewClient should install a cookie jar")
	}
	if _, ok := c.http.Transport.(*utlsRoundTripper); !ok {
		t.Fatalf("transport type = %T", c.http.Transport)
	}

	tracker := &closeTrackingTransport{}
	c = &Client{http: &http.Client{Transport: tracker}}
	c.Close()
	if !tracker.closed {
		t.Fatal("Close should close idle transport connections")
	}
	(*Client)(nil).Close()
	(&Client{}).Close()

	ctx, cancel := context.WithCancel(context.Background())
	c = newTestClient(func(*http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, "", nil), nil
	})
	c.ctx = ctx
	req, err := c.newReq(http.MethodPost, "/backend-api/test", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("newReq returned err: %v", err)
	}
	if req.Context() != ctx {
		t.Fatal("newReq should use client request context")
	}
	if req.Header.Get("Authorization") != "Bearer access-token" {
		t.Fatalf("Authorization header = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Openai-Target-Path") != "/backend-api/test" {
		t.Fatalf("target path header = %q", req.Header.Get("X-Openai-Target-Path"))
	}
	cancel()
	if err := sleepContext(ctx, time.Hour); err == nil {
		t.Fatal("sleepContext should return canceled context error")
	}
}

func TestDoNoRedirectAndBootstrap(t *testing.T) {
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case BaseURL + "/redirect":
			return imgenHTTPResponse(http.StatusFound, "", http.Header{"Location": []string{BaseURL + "/next"}}), nil
		case BaseURL + "/":
			return imgenHTTPResponse(http.StatusOK, "ok", http.Header{"Set-Cookie": []string{"oai-did=did; Path=/"}}), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})
	req, err := c.newReq(http.MethodGet, "/redirect", nil)
	if err != nil {
		t.Fatalf("newReq: %v", err)
	}
	resp, err := c.doNoRedirect(req)
	if err != nil {
		t.Fatalf("doNoRedirect returned err: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("doNoRedirect status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if err := c.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap returned err: %v", err)
	}
	u, _ := url.Parse(BaseURL)
	if got := c.http.Jar.Cookies(u); len(got) == 0 {
		t.Fatal("Bootstrap should retain cookies in jar")
	}
}

func TestReadLimitedBodies(t *testing.T) {
	if _, err := readLimitedErrorBody(errReader{}); err == nil {
		t.Fatal("readLimitedErrorBody should return reader errors")
	}
	if _, err := readLimitedImageBody(errReader{}); err == nil {
		t.Fatal("readLimitedImageBody should return reader errors")
	}
	data, err := readLimitedImageBody(strings.NewReader("image-bytes"))
	if err != nil {
		t.Fatalf("readLimitedImageBody returned err: %v", err)
	}
	if string(data) != "image-bytes" {
		t.Fatalf("image body = %q", data)
	}
}

func TestSentinelRequirementsPingAndHeartbeat(t *testing.T) {
	var pingCount int32
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/sentinel/chat-requirements":
			if req.Method != http.MethodPost {
				t.Fatalf("chat-requirements method = %s", req.Method)
			}
			return imgenHTTPResponse(http.StatusOK, `{"token":"chat-token","proofofwork":{"required":true,"seed":"seed","difficulty":"f"}}`, nil), nil
		case "/backend-api/sentinel/ping":
			atomic.AddInt32(&pingCount, 1)
			return imgenHTTPResponse(http.StatusOK, `{}`, nil), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})

	reqs, err := c.getChatRequirements()
	if err != nil {
		t.Fatalf("getChatRequirements returned err: %v", err)
	}
	if reqs.ChatToken != "chat-token" || !strings.HasPrefix(reqs.ProofToken, "gAAAAAB") {
		t.Fatalf("requirements = %#v", reqs)
	}
	if err := c.sentinelPing(); err != nil {
		t.Fatalf("sentinelPing returned err: %v", err)
	}

	stop := c.startHeartbeat(time.Millisecond)
	deadline := time.Now().Add(200 * time.Millisecond)
	for atomic.LoadInt32(&pingCount) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	if atomic.LoadInt32(&pingCount) < 2 {
		t.Fatalf("heartbeat ping count = %d", pingCount)
	}

	failing := newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusForbidden, "denied", nil), nil
	})
	if _, err := failing.getChatRequirements(); err == nil {
		t.Fatal("non-200 chat-requirements should fail")
	}
	if err := failing.sentinelPing(); err == nil {
		t.Fatal("non-200 sentinel ping should fail")
	}
}

func TestProofTokenHelpers(t *testing.T) {
	if got := SolveProofToken("", "f", DefaultUA); got != "" {
		t.Fatalf("empty seed proof token = %q", got)
	}
	if got := SolveProofToken("seed", "", DefaultUA); got != "" {
		t.Fatalf("empty difficulty proof token = %q", got)
	}
	if got := SolveProofToken("seed", "f", DefaultUA); !strings.HasPrefix(got, "gAAAAAB") {
		t.Fatalf("proof token = %q", got)
	}
	if got := GenerateRequirementsToken(DefaultUA); !strings.HasPrefix(got, "gAAAAAC") {
		t.Fatalf("requirements token = %q", got)
	}
	if got := randHex(7); len(got) != 7 {
		t.Fatalf("randHex len = %d", len(got))
	}
}
