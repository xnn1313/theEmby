package gateway

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// sanitizeURL 返回脱敏后的 URL 字符串（api_key 打码），专用于日志输出。
// 日志里出现真实 API Key 即 bug。
func sanitizeURL(u *url.URL) string {
	c := *u
	q := c.Query()
	if q.Has("api_key") {
		q.Set("api_key", "***")
	}
	c.RawQuery = q.Encode()
	return c.String()
}

// ensureAPIKey 若 query 缺少 api_key 则补上服务 Key；
// 客户端自带的 key 优先保留，不覆盖。
func ensureAPIKey(q url.Values, apiKey string) url.Values {
	if !q.Has("api_key") {
		q.Set("api_key", apiKey)
	}
	return q
}

// newReverseProxy 构造透传反代：
//   - 保留 path / query / headers（客户端自带的 Emby 认证头原样转发，不剥离）；
//   - 缺 api_key 时补服务 Key；
//   - 剥离网关内部头 X-NB-User（不上游）。
func newReverseProxy(target *url.URL, apiKey string, transport *http.Transport, logger *log.Logger) *httputil.ReverseProxy {
	director := func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		q := ensureAPIKey(r.URL.Query(), apiKey)
		r.URL.RawQuery = q.Encode()
		r.Header.Del("X-NB-User")
		r.Host = target.Host
	}
	return &httputil.ReverseProxy{
		Director:  director,
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// 脱敏后打日志：绝不输出 API Key。
			logger.Printf("proxy error %s %s: %v", r.Method, sanitizeURL(r.URL), err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
}
