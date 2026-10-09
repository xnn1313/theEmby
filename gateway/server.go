package gateway

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"nextemby-replay/engine"
	"nextemby-replay/store"
)

// Server 是反代网关：透传上游 Emby，只劫持 PlaybackInfo 走播放决策引擎。
type Server struct {
	cfg      Config
	upstream *url.URL
	proxy    *httputil.ReverseProxy
	client   *http.Client // 外发 HTTP（带 egress CA 的 Transport）
	engine   *engine.Engine
	drives   map[string]engine.DriveClient // accountID -> 网盘 client（/nb/stream 现取直链用）
	finger   FingerprintResolver
	hmacKey  []byte
	logger   *log.Logger

	mu          sync.Mutex
	streamCache map[string]streamCacheEntry // token -> 直链（10min）

	webUI *webUIState // 管理后台 + 个人中心（EnableWebUI 启用，未启用时路由不存在）

	// db 是 SQLite 持久化存储（AttachStore 接入，未接入时为 nil，
	// 管理后台/个人中心回退到 engine 内存视图，保证既有测试零改动）。
	db *store.Store
}

// NewServer 组装网关。logger 为 nil 时用默认输出。
func NewServer(cfg Config, eng *engine.Engine, drives map[string]engine.DriveClient, finger FingerprintResolver, logger *log.Logger) (*Server, error) {
	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	transport, err := newTransport(cfg.CertFile)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		cfg:         cfg,
		upstream:    target,
		client:      &http.Client{Transport: transport},
		engine:      eng,
		drives:      drives,
		finger:      finger,
		hmacKey:     cfg.HMACKey,
		logger:      logger,
		streamCache: map[string]streamCacheEntry{},
	}
	s.proxy = newReverseProxy(target, cfg.APIKey, transport, logger)
	return s, nil
}

// AttachStore 接入 SQLite 持久化：管理后台的统计/用户/播放日志与
// 个人中心的统计改读 DB（PoolLocks/并发会话仍读 engine 内存，见 README）。
// 未调用时各 handler 回退到 engine 内存视图。
func (s *Server) AttachStore(db *store.Store) {
	s.db = db
}

// Handler 返回路由：PlaybackInfo 劫持（/emby 前缀与根路径都支持）、
// /nb/stream 直链跳转，其余全部透传。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /emby/Items/{id}/PlaybackInfo", s.handlePlaybackInfo)
	mux.HandleFunc("POST /emby/Videos/{id}/PlaybackInfo", s.handlePlaybackInfo)
	mux.HandleFunc("POST /Items/{id}/PlaybackInfo", s.handlePlaybackInfo)
	mux.HandleFunc("POST /Videos/{id}/PlaybackInfo", s.handlePlaybackInfo)
	mux.HandleFunc("GET /nb/stream", s.handleStream)
	if s.webUI != nil {
		// 管理后台（鉴权）
		mux.HandleFunc("GET /nb/admin/", s.handleAdminPage)
		mux.HandleFunc("GET /nb/admin/api/stats", s.requireAdmin(s.handleAdminStats))
		mux.HandleFunc("GET /nb/admin/api/users", s.requireAdmin(s.handleAdminUsers))
		mux.HandleFunc("PUT /nb/admin/api/users/{id}", s.requireAdmin(s.handleAdminUpdateUser))
		mux.HandleFunc("GET /nb/admin/api/pool", s.requireAdmin(s.handleAdminPool))
		mux.HandleFunc("POST /nb/admin/api/pool/{id}/health", s.requireAdmin(s.handleAdminPoolHealth))
		mux.HandleFunc("GET /nb/admin/api/decisions", s.requireAdmin(s.handleAdminDecisions))
		mux.HandleFunc("GET /nb/admin/api/config", s.requireAdmin(s.handleAdminConfig))
		// 个人中心（用户自助，身份经 ?nb_user=）
		mux.HandleFunc("GET /nb/me/", s.handleMePage)
		mux.HandleFunc("GET /nb/me/api/profile", s.handleMeProfile)
		mux.HandleFunc("POST /nb/me/api/cookie", s.handleMeCookie)
		mux.HandleFunc("POST /nb/me/api/mode", s.handleMeMode)
	}
	mux.Handle("/", s.proxy)
	return mux
}
