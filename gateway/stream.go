package gateway

import (
	"net/http"
	"time"
)

// streamURLCacheTTL 是网关侧直链缓存 TTL（对标引擎：10 分钟，
// 在 115 直链有效期内做短缓存，省"取直链" API 调用）。
const streamURLCacheTTL = 10 * time.Minute

type streamCacheEntry struct {
	url     string
	expires time.Time
}

// rememberStreamURL 记住 token → 引擎决策出的直链（10min）。
func (s *Server) rememberStreamURL(token, directURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamCache == nil {
		s.streamCache = map[string]streamCacheEntry{}
	}
	s.streamCache[token] = streamCacheEntry{url: directURL, expires: time.Now().Add(streamURLCacheTTL)}
}

// streamCacheGet 命中返回缓存直链；过期/不存在返回 false（惰性淘汰）。
func (s *Server) streamCacheGet(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.streamCache[token]
	if !ok || time.Now().After(e.expires) {
		delete(s.streamCache, token)
		return "", false
	}
	return e.url, true
}

// handleStream 处理 /nb/stream?t=<token>：
// 验签 → 网关 10min 缓存命中则 302；未命中则经 DriveClient 现取 → 302 到 115 直链。
// 用 302 而不是代理转发：省网关带宽，Emby 客户端跟随跳转。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	claims, err := verifyStreamToken(s.hmacKey, token, time.Now())
	if err != nil {
		s.logger.Printf("stream token 无效: %v", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if u, ok := s.streamCacheGet(token); ok {
		http.Redirect(w, r, u, http.StatusFound)
		return
	}

	drv, ok := s.drives[claims.Acct]
	if !ok {
		s.logger.Printf("stream 未知账号 acct=%s", claims.Acct)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	u, err := drv.DirectURL(r.Context(), claims.SHA1)
	if err != nil {
		s.logger.Printf("stream 取直链失败 acct=%s sha1=%.12s: %v", claims.Acct, claims.SHA1, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	s.rememberStreamURL(token, u)
	http.Redirect(w, r, u, http.StatusFound)
}
