package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// Only fixture tests opt into local HTTP endpoints; production has no switch
// that disables download address validation.
func useLocalImageDownloadClient(t *testing.T) {
	t.Helper()
	previous := imageDownloadHTTPClient
	imageDownloadHTTPClient = &http.Client{Timeout: time.Second}
	t.Cleanup(func() { imageDownloadHTTPClient = previous })
}

func TestImageDownloadRejectsPrivateAndReservedAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.64.0.1", "0.1.2.3", "224.0.0.1", "255.255.255.255", "::1", "::ffff:127.0.0.1", "fd00::1", "fe80::1", "fec0::1", "64:ff9b::7f00:1"} {
		if publicImageAddress(netip.MustParseAddr(address)) {
			t.Errorf("allowed %s", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"} {
		if !publicImageAddress(netip.MustParseAddr(address)) {
			t.Errorf("rejected public address %s", address)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "http://localhost/", "http://x.localhost/", "http://user:pass@example.com/", "http://[::1]/"} {
		u, err := url.Parse(raw)
		if err == nil && validateImageDownloadURL(u) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	response, err := newImageDownloadHTTPClient().Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || hits.Load() != 0 {
		t.Fatal("protected endpoint was contacted")
	}
}

func TestImageDNSValidationPinsNumericDialAndRejectsMixedAnswers(t *testing.T) {
	dials := 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		dials++
		return nil, fmt.Errorf("unexpected dial %s", address)
	}
	if _, err := dialPublicImageAddress(t.Context(), "tcp", "example.com:443", lookup, dial); err == nil || dials != 0 {
		t.Fatal("mixed DNS answer reached dial")
	}
	lookups := 0
	lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dial = func(_ context.Context, _ string, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("unvalidated hostname reached dial: %s", address)
		}
		return nil, errors.New("fixture stops before network")
	}
	_, _ = dialPublicImageAddress(t.Context(), "tcp", "example.com:443", lookup, dial)
	if lookups != 1 {
		t.Fatal("DNS resolved more than once")
	}
}

func TestImageRedirectCannotReachPrivateDestination(t *testing.T) {
	var privateHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { privateHits.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	client := newImageDownloadHTTPClient()
	transport := client.Transport.(imageDownloadTransport).transport
	// Simulate the initial public host only, then use the real redirect policy.
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	response, err := client.Get("http://public.example/image")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || privateHits.Load() != 0 {
		t.Fatal("redirect reached private endpoint")
	}
}

type imageContextTransport struct{}

func (imageContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestImageReferenceReadPropagatesRequestCancellation(t *testing.T) {
	previous := imageDownloadHTTPClient
	imageDownloadHTTPClient = &http.Client{Transport: imageContextTransport{}}
	defer func() { imageDownloadHTTPClient = previous }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := readImageRefBytes("https://example.com/image", 0, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
