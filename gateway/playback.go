package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"nextemby-replay/engine"
)

// userIDFromRequest 取 NextEmby 侧用户身份：
// X-NB-User header 优先，其次 ?nb_user= 参数，缺省 "guest"。
func userIDFromRequest(r *http.Request) string {
	if u := r.Header.Get("X-NB-User"); u != "" {
		return u
	}
	if u := r.URL.Query().Get("nb_user"); u != "" {
		return u
	}
	return "guest"
}

// clientFromEmbyAuth 从 X-Emby-Authorization 头里提取设备标识，
// 如 MediaBrowser Client="...", Device="Filmly", ... → "Filmly"。
func clientFromEmbyAuth(r *http.Request) string {
	h := r.Header.Get("X-Emby-Authorization")
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "Device=") {
			return strings.Trim(strings.TrimPrefix(part, "Device="), `"`)
		}
	}
	return ""
}

// fetchUpstreamPlaybackInfo 把 PlaybackInfo 请求原样转发给上游（含 body），
// 返回上游状态码与 body。api_key 缺失时补服务 Key；nb_user / X-NB-User 不上游。
func (s *Server) fetchUpstreamPlaybackInfo(r *http.Request) (int, []byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("gateway: 读取请求 body 失败: %w", err)
	}

	u := *r.URL
	u.Scheme = s.upstream.Scheme
	u.Host = s.upstream.Host
	q := ensureAPIKey(u.Query(), s.cfg.APIKey)
	q.Del("nb_user")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, vv := range r.Header {
		if strings.EqualFold(k, "X-NB-User") {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	req.Host = s.upstream.Host

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// handlePlaybackInfo 劫持 PlaybackInfo：
//  1. 先用服务 Key 调上游拿到 MediaSources；
//  2. 经 FingerprintResolver 解析 SHA1，查不到 → 优雅降级，原样返回上游响应；
//  3. 查到 → 调 Engine.Handle 做播放决策；
//  4. 成功 → 把首个可解析源的 DirectStreamUrl 改写为 /nb/stream?t=<token>；
//     并发拒绝 → 429；引擎异常 → 降级透传。
//
// MVP 说明：一个 PlaybackInfo 请求只对首个可解析的 MediaSource 跑一次决策
// （= 占用一个并发会话），其余源保持上游原样作 fallback。
func (s *Server) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logURL := sanitizeURL(r.URL)

	userID := userIDFromRequest(r)

	// 播放前置检查（仅 DB 接入时）：封禁/过期/模板日额度。命中直接拒绝，不走上游。
	if s.db != nil {
		if code := s.checkPlayAllowed(ctx, userID); code != "" {
			status := http.StatusTooManyRequests
			if code == "user_banned" || code == "user_expired" {
				status = http.StatusForbidden
			}
			s.logger.Printf("playbackinfo 前置拒绝 user=%s error=%s", userID, code)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
			return
		}
	}

	status, upBody, err := s.fetchUpstreamPlaybackInfo(r)
	if err != nil {
		s.logger.Printf("playbackinfo 上游失败 %s: %v", logURL, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	if status < 200 || status >= 300 {
		// 上游非 2xx：原样透传状态码与 body。
		w.WriteHeader(status)
		_, _ = w.Write(upBody)
		return
	}

	var doc map[string]any
	if err := json.Unmarshal(upBody, &doc); err != nil {
		// 上游响应不是 JSON：原样透传。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(upBody)
		return
	}
	rawSources, _ := doc["MediaSources"].([]any)
	if len(rawSources) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(upBody)
		return
	}

	for _, rs := range rawSources {
		src, ok := rs.(map[string]any)
		if !ok {
			continue
		}
		srcPath, _ := src["Path"].(string)
		name, _ := src["Name"].(string)
		var size int64
		if f, ok := src["Size"].(float64); ok {
			size = int64(f)
		}

		// 路径映射 + 快照索引解析 SHA1；查不到 → 这个源保持上游原样（优雅降级）。
		sha1, ok := s.finger.ResolveSHA1(ctx, srcPath)
		if !ok {
			continue
		}
		if name == "" {
			name = path.Base(srcPath)
		}

		d, err := s.engine.Handle(ctx, engine.PlaybackRequest{
			UserID:   userID,
			FileSHA1: sha1,
			FileName: name,
			FileSize: size,
			Client:   clientFromEmbyAuth(r),
			UA:       r.UserAgent(),
		})
		if err != nil {
			// 引擎异常（如未知用户）：降级透传，不中断播放。
			s.logger.Printf("playbackinfo 引擎异常 user=%s sha1=%.12s: %v（降级透传）", userID, sha1, err)
			break
		}
		if !d.Allowed {
			s.logger.Printf("playbackinfo 拒绝 user=%s branch=%s deny=%s", userID, d.Branch, d.DenyReason)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":  "concurrency_limit",
				"detail": d.DenyReason,
			})
			return
		}

		token := issueStreamToken(s.hmacKey, d.AccountID, sha1, time.Now())
		s.rememberStreamURL(token, StreamInfo{
			URL:       d.DirectURL,
			UserID:    userID,
			SHA1:      sha1,
			AccountID: d.AccountID,
			Filename:  name,
			UA:        r.UserAgent(),
		})
		src["DirectStreamUrl"] = "/nb/stream?t=" + token

		// MVP：会话保持到 token 过期后释放；后续可接 Sessions/Playing/Stopped 做精确释放。
		if d.Release != nil {
			release := d.Release
			time.AfterFunc(streamTokenExpiry, release)
		}
		s.logger.Printf("playbackinfo user=%s branch=%s account=%s sha1=%.12s", userID, d.Branch, d.AccountID, sha1)
		break // 只决策首个可解析源
	}

	out, err := json.Marshal(doc)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// checkPlayAllowed 是播放前置检查（仅 DB 接入时调用）：
// banned → user_banned；过期 → user_expired；模板日额度超限 → daily_limit /
// daily_rapid_limit。返回 "" 表示放行。用户不存在时不拦截（引擎会降级透传）。
func (s *Server) checkPlayAllowed(ctx context.Context, userID string) string {
	user, err := s.engine.Users().GetUser(ctx, userID)
	if err != nil {
		return ""
	}
	if user.Banned {
		s.logUser("用户 %s 已被禁用，拒绝播放", userID)
		return "user_banned"
	}
	if user.ExpiresAt != 0 && time.Now().Unix() > user.ExpiresAt {
		s.logUser("用户 %s 已过期，拒绝播放", userID)
		return "user_expired"
	}
	t, found, err := s.db.GetTemplate(ctx, user.Template)
	if err != nil || !found {
		return ""
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	if t.DailyPlays > 0 {
		n, err := s.db.CountUserDecisionsSince(ctx, userID, midnight)
		if err == nil && n >= t.DailyPlays {
			s.logUser("用户 %s 触发今日播放上限（%d 次），拒绝播放", userID, t.DailyPlays)
			return "daily_limit"
		}
	}
	if t.DailyRapid > 0 {
		n, err := s.db.CountUserRapidSince(ctx, userID, midnight)
		if err == nil && n >= t.DailyRapid {
			s.logUser("用户 %s 触发今日秒传上限（%d 次），拒绝播放", userID, t.DailyRapid)
			return "daily_rapid_limit"
		}
	}
	return ""
}

// handleStopped 处理客户端的 Sessions/Playing/Stopped 信号：
// 只打一条 play 日志，然后原样透传给上游（不吞掉）。会话释放仍由现有
// token 过期机制处理，这里不做任何"虚拟会话"清理。
func (s *Server) handleStopped(w http.ResponseWriter, r *http.Request) {
	client := clientFromEmbyAuth(r)
	user := userIDFromRequest(r)
	s.logPlay("收到客户端 Stopped 信号 (%s, %s)", client, user)
	s.proxy.ServeHTTP(w, r)
}
