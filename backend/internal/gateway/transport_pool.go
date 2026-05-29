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
	transportPoolMaxEntries      = 100000
	transportPoolIdleTTL         = 2 * time.Hour
	transportPoolCleanupInterval = 10 * time.Minute
)

type transportPoolEntry struct {
	transport  *http.Transport
	lastUsedAt time.Time
}

// TransportPool 按账户+代理隔离的 HTTP Transport 连接池
// 确保不同账户的连接互不干扰，同一账户的连接可以复用
type TransportPool struct {
	mu              sync.RWMutex
	transports      map[string]*transportPoolEntry // key = poolKey(accountID, proxyURL)
	lastCleanupTime time.Time
}

// NewTransportPool 创建连接池
func NewTransportPool() *TransportPool {
	return &TransportPool{
		transports:      make(map[string]*transportPoolEntry),
		lastCleanupTime: time.Now(),
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

	// 快路径：读锁检查
	p.mu.RLock()
	entry, ok := p.transports[key]
	p.mu.RUnlock()
	if ok {
		p.touch(key, now)
		return entry.transport
	}

	// 慢路径：写锁创建
	p.mu.Lock()
	defer p.mu.Unlock()

	// 双重检查
	if entry, ok = p.transports[key]; ok {
		entry.lastUsedAt = now
		return entry.transport
	}

	p.cleanupIdleLocked(now)
	if len(p.transports) >= transportPoolMaxEntries {
		p.deleteOldestLocked()
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

	p.transports[key] = &transportPoolEntry{transport: t, lastUsedAt: now}
	return t
}

func (p *TransportPool) touch(key string, now time.Time) {
	p.mu.Lock()
	if entry, ok := p.transports[key]; ok {
		entry.lastUsedAt = now
	}
	p.cleanupIdleLocked(now)
	p.mu.Unlock()
}

func (p *TransportPool) cleanupIdleLocked(now time.Time) {
	if now.Sub(p.lastCleanupTime) < transportPoolCleanupInterval && len(p.transports) < transportPoolMaxEntries {
		return
	}
	for key, entry := range p.transports {
		if now.Sub(entry.lastUsedAt) > transportPoolIdleTTL {
			entry.transport.CloseIdleConnections()
			delete(p.transports, key)
		}
	}
	p.lastCleanupTime = now
}

func (p *TransportPool) deleteOldestLocked() {
	var oldestKey string
	var oldestAt time.Time
	for key, entry := range p.transports {
		if oldestKey == "" || entry.lastUsedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.lastUsedAt
		}
	}
	if oldestKey != "" {
		p.transports[oldestKey].transport.CloseIdleConnections()
		delete(p.transports, oldestKey)
	}
}

// CloseIdle 关闭所有 Transport 的空闲连接
func (p *TransportPool) CloseIdle() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.transports {
		entry.transport.CloseIdleConnections()
	}
	p.cleanupIdleLocked(now)
}

// RemoveAccount 移除指定账户的 Transport（账户被禁用时清理）
func (p *TransportPool) RemoveAccount(accountID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	prefix1 := "direct:" + itoa(accountID)
	prefix2 := ":" + itoa(accountID)

	for key, entry := range p.transports {
		if key == prefix1 || strings.HasSuffix(key, prefix2) {
			entry.transport.CloseIdleConnections()
			delete(p.transports, key)
		}
	}
}
