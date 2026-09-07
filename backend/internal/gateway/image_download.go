package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var imageReservedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("100::/64"),
}

func publicImageAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range imageReservedRanges {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func validateImageDownloadURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("图片地址必须是不含认证信息的 HTTP(S) URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("图片地址不能指向本机或私网")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicImageAddress(ip) {
		return fmt.Errorf("图片地址不能指向本机、私网或保留地址")
	}
	return nil
}

type imageDownloadTransport struct{ transport *http.Transport }

func (t imageDownloadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validateImageDownloadURL(req.URL); err != nil {
		return nil, err
	}
	return t.transport.RoundTrip(req)
}

type imageAddressLookup func(context.Context, string, string) ([]netip.Addr, error)
type imageAddressDial func(context.Context, string, string) (net.Conn, error)

func dialPublicImageAddress(ctx context.Context, network, address string, lookup imageAddressLookup, dial imageAddressDial) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := lookup(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("解析图片地址失败: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("图片地址没有可用 IP")
	}
	for _, ip := range addresses {
		if !publicImageAddress(ip) {
			return nil, fmt.Errorf("图片域名解析到受保护地址")
		}
	}
	var lastErr error
	for _, ip := range addresses {
		// Dial the validated numeric address, never resolve the hostname a second
		// time. TLS still verifies the original request hostname in Transport.
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func newImageDownloadHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Environment proxies must not bypass address validation.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialPublicImageAddress(ctx, network, address, net.DefaultResolver.LookupNetIP, dialer.DialContext)
		},
		ForceAttemptHTTP2: true, MaxIdleConns: 64, MaxIdleConnsPerHost: 4,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}
	return &http.Client{
		Transport: imageDownloadTransport{transport}, Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("图片下载重定向次数过多")
			}
			return validateImageDownloadURL(req.URL)
		},
	}
}

func imageRequestContext(contexts []context.Context) context.Context {
	if len(contexts) > 0 && contexts[0] != nil {
		return contexts[0]
	}
	return context.Background()
}
