package gateway

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	transportPoolMaxEntries = 100000
	transportPoolIdleTTL    = 2 * time.Hour
)

type transportPoolEntry struct {
	transport  *http.Transport
	lastUsedAt time.Time
	expiry     *cacheExpiry
}

// TransportPool 按账户+代理隔离的 HTTP Transport 连接池
// 确保不同账户的连接互不干扰，同一账户的连接可以复用
type TransportPool struct {
	mu         sync.RWMutex
	transports map[string]*transportPoolEntry // key = poolKey(accountID, proxyURL)
	expiries   cacheExpiryQueue
}

// NewTransportPool 创建连接池
func NewTransportPool() *TransportPool {
	return &TransportPool{
		transports: make(map[string]*transportPoolEntry),
	}
}

// poolKey 生成连接池 key：按账户ID + 代理URL 隔离
// 相同账户使用相同代理时复用连接，不同代理则隔离
func poolKey(accountID int64, proxyURL string) string {
	if proxyURL == "" {
		return "direct:" + itoa(accountID)
	}
	return "proxy:" + proxyURL + ":" + itoa(accountID)
}

// itoa 简单的 int64 转字符串，避免 import strconv
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// GetTransport 获取或创建指定账户的 Transport
func (p *TransportPool) GetTransport(accountID int64, proxyURL string) *http.Transport {
	key := poolKey(accountID, proxyURL)
	now := time.Now()

	p.mu.Lock()
	var closing []*http.Transport
	defer func() {
		p.mu.Unlock()
		for _, transport := range closing {
			transport.CloseIdleConnections()
		}
	}()

	// 双重检查
	if entry, ok := p.transports[key]; ok {
		entry.lastUsedAt = now
		p.expiries.update(entry.expiry, now)
		closing = p.cleanupIdleLocked(now)
		return entry.transport
	}

	closing = p.cleanupIdleLocked(now)
	if len(p.transports) >= transportPoolMaxEntries {
		if old := p.deleteOldestLocked(); old != nil {
			closing = append(closing, old)
		}
	}

	t := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(parsed)
		}
	}

	p.putLocked(key, &transportPoolEntry{transport: t, lastUsedAt: now})
	return t
}

func (p *TransportPool) putLocked(key string, entry *transportPoolEntry) {
	entry.expiry = p.expiries.add(key, entry.lastUsedAt)
	p.transports[key] = entry
}

func (p *TransportPool) cleanupIdleLocked(now time.Time) []*http.Transport {
	var closing []*http.Transport
	for len(closing) < cacheCleanupBudget && len(p.expiries) > 0 && now.Sub(p.expiries[0].at) > transportPoolIdleTTL {
		closing = append(closing, p.deleteOldestLocked())
	}
	return closing
}

func (p *TransportPool) deleteOldestLocked() *http.Transport {
	if len(p.expiries) == 0 {
		return nil
	}
	oldest := p.expiries[0]
	transport := p.transports[oldest.key].transport
	delete(p.transports, oldest.key)
	p.expiries.remove(oldest)
	return transport
}

// CloseIdle 关闭所有 Transport 的空闲连接
func (p *TransportPool) CloseIdle() {
	now := time.Now()
	p.mu.Lock()
	closing := p.cleanupIdleLocked(now)
	for _, entry := range p.transports {
		closing = append(closing, entry.transport)
	}
	p.mu.Unlock()
	for _, transport := range closing {
		transport.CloseIdleConnections()
	}
}

// RemoveAccount 移除指定账户的 Transport（账户被禁用时清理）
func (p *TransportPool) RemoveAccount(accountID int64) {
	p.mu.Lock()
	var closing []*http.Transport

	prefix1 := "direct:" + itoa(accountID)
	prefix2 := ":" + itoa(accountID)

	for key, entry := range p.transports {
		if key == prefix1 || strings.HasSuffix(key, prefix2) {
			closing = append(closing, entry.transport)
			p.expiries.remove(entry.expiry)
			delete(p.transports, key)
		}
	}
	p.mu.Unlock()
	for _, transport := range closing {
		transport.CloseIdleConnections()
	}
}
