package gateway

import (
	"context"
	"net/http"
	"sort"
	"time"
)

// streamURLCacheTTL 是网关侧直链缓存 TTL（对标引擎：10 分钟，
// 在 115 直链有效期内做短缓存，省"取直链" API 调用）。
const streamURLCacheTTL = 10 * time.Minute

type streamCacheEntry struct {
	url       string
	expires   time.Time
	userID    string
	sha1      string
	accountID string
	filename  string
	ua        string
}

// StreamInfo 是直链缓存条目的元数据（管理后台"缓存列表"展示用）。
type StreamInfo struct {
	URL       string
	UserID    string
	SHA1      string
	AccountID string
	Filename  string
	UA        string
}

// rememberStreamURL 记住 token → 引擎决策出的直链（10min）。
func (s *Server) rememberStreamURL(token string, info StreamInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamCache == nil {
		s.streamCache = map[string]streamCacheEntry{}
	}
	s.streamCache[token] = streamCacheEntry{
		url:       info.URL,
		expires:   time.Now().Add(streamURLCacheTTL),
		userID:    info.UserID,
		sha1:      info.SHA1,
		accountID: info.AccountID,
		filename:  info.Filename,
		ua:        info.UA,
	}
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

// CacheEntryJSON 是直链缓存条目的管理后台展示形态。
type CacheEntryJSON struct {
	User       string `json:"user"`
	Filename   string `json:"filename"`
	AccountUID string `json:"accountUID"`
	UA         string `json:"ua"`
	ExpiresAt  int64  `json:"expiresAt"`
	SHA1       string `json:"sha1"`
}

// ListCache 返回未过期的直链缓存条目（过期条目惰性淘汰）。
// AccountUID 经 accounts115 按 AccountID 查 uid；DB 未接入时为 ""。
func (s *Server) ListCache() []CacheEntryJSON {
	uidByAccount := map[string]string{}
	if s.db != nil {
		if as, err := s.db.Accounts115(s.cfg.CookieKey); err == nil {
			if accs, err := as.ListAccounts115(context.Background()); err == nil {
				for _, a := range accs {
					uidByAccount[a.ID] = a.UID
				}
			}
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CacheEntryJSON{}
	for tok, e := range s.streamCache {
		if now.After(e.expires) {
			delete(s.streamCache, tok) // 惰性淘汰
			continue
		}
		out = append(out, CacheEntryJSON{
			User:       e.userID,
			Filename:   e.filename,
			AccountUID: uidByAccount[e.accountID],
			UA:         e.ua,
			ExpiresAt:  e.expires.Unix(),
			SHA1:       e.sha1,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresAt != out[j].ExpiresAt {
			return out[i].ExpiresAt < out[j].ExpiresAt
		}
		return out[i].User < out[j].User
	})
	return out
}

// ClearCache 清空直链缓存。
func (s *Server) ClearCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCache = map[string]streamCacheEntry{}
}

// DeleteCache 删除指定 (user, sha1) 的缓存条目；命中返回 true。
func (s *Server) DeleteCache(user, sha1 string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, e := range s.streamCache {
		if e.userID == user && e.sha1 == sha1 {
			delete(s.streamCache, tok)
			return true
		}
	}
	return false
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
	s.rememberStreamURL(token, StreamInfo{URL: u, SHA1: claims.SHA1, AccountID: claims.Acct})
	http.Redirect(w, r, u, http.StatusFound)
}
