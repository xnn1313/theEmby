package gateway

import (
	"bytes"
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

	userID := userIDFromRequest(r)
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
		s.rememberStreamURL(token, d.DirectURL)
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
