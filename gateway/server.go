package gateway

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
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

	// sink 是 engine.LogFunc 与网关事件的异步落库通道（AttachLogSink 接入）。
	sink *logSink

	pathMu    sync.RWMutex
	pathRules []PathRule // 路径映射规则内存缓存（ReloadPathMaps 从 DB/环境变量重建）
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
	// 客户端 Stopped 信号：只打日志，继续透传上游（会话仍由 token 过期释放）。
	mux.HandleFunc("POST /Sessions/Playing/Stopped", s.handleStopped)
	mux.HandleFunc("POST /emby/Sessions/Playing/Stopped", s.handleStopped)
	if s.webUI != nil {
		// 管理后台（鉴权）
		mux.HandleFunc("GET /nb/admin/", s.handleAdminPage)
		mux.HandleFunc("POST /nb/admin/api/login", s.handleLogin)             // 不鉴权
		mux.HandleFunc("GET /nb/admin/api/login/status", s.handleLoginStatus) // 不鉴权
		mux.HandleFunc("POST /nb/admin/api/logout", s.handleLogout)           // 不鉴权
		mux.HandleFunc("GET /nb/admin/api/stats", s.requireAdmin(s.handleAdminStats))
		mux.HandleFunc("GET /nb/admin/api/users", s.requireAdmin(s.handleAdminUsers))
		mux.HandleFunc("POST /nb/admin/api/users", s.requireAdmin(s.handleAdminUserCreate))
		mux.HandleFunc("PUT /nb/admin/api/users/{id}", s.requireAdmin(s.handleAdminUpdateUser))
		mux.HandleFunc("DELETE /nb/admin/api/users/{id}", s.requireAdmin(s.handleAdminUserDelete))
		mux.HandleFunc("GET /nb/admin/api/pool", s.requireAdmin(s.handleAdminPool))
		mux.HandleFunc("POST /nb/admin/api/pool/{id}/health", s.requireAdmin(s.handleAdminPoolHealth))
		mux.HandleFunc("GET /nb/admin/api/decisions", s.requireAdmin(s.handleAdminDecisions))
		mux.HandleFunc("GET /nb/admin/api/config", s.requireAdmin(s.handleAdminConfig))
		mux.HandleFunc("GET /nb/admin/api/settings", s.requireAdmin(s.handleAdminSettings))
		mux.HandleFunc("PUT /nb/admin/api/settings", s.requireAdmin(s.handleAdminSettingsPut))
		mux.HandleFunc("GET /nb/admin/api/accounts", s.requireAdmin(s.handleAdminAccounts))
		mux.HandleFunc("POST /nb/admin/api/accounts", s.requireAdmin(s.handleAdminAccountCreate))
		mux.HandleFunc("PUT /nb/admin/api/accounts/{id}", s.requireAdmin(s.handleAdminAccountUpdate))
		mux.HandleFunc("DELETE /nb/admin/api/accounts/{id}", s.requireAdmin(s.handleAdminAccountDelete))
		mux.HandleFunc("GET /nb/admin/api/pathmaps", s.requireAdmin(s.handleAdminPathMaps))
		mux.HandleFunc("POST /nb/admin/api/pathmaps", s.requireAdmin(s.handleAdminPathMapCreate))
		mux.HandleFunc("DELETE /nb/admin/api/pathmaps/{embyPath}", s.requireAdmin(s.handleAdminPathMapDelete))
		mux.HandleFunc("GET /nb/admin/api/templates", s.requireAdmin(s.handleAdminTemplates))
		mux.HandleFunc("POST /nb/admin/api/templates", s.requireAdmin(s.handleAdminTemplateCreate))
		mux.HandleFunc("PUT /nb/admin/api/templates/{name}", s.requireAdmin(s.handleAdminTemplateUpdate))
		mux.HandleFunc("DELETE /nb/admin/api/templates/{name}", s.requireAdmin(s.handleAdminTemplateDelete))
		mux.HandleFunc("GET /nb/admin/api/logs", s.requireAdmin(s.handleAdminLogs))
		mux.HandleFunc("DELETE /nb/admin/api/logs", s.requireAdmin(s.handleAdminLogsClear))
		mux.HandleFunc("GET /nb/admin/api/cache", s.requireAdmin(s.handleAdminCache))
		mux.HandleFunc("DELETE /nb/admin/api/cache", s.requireAdmin(s.handleAdminCacheClear))
		mux.HandleFunc("DELETE /nb/admin/api/cache/{user}/{sha1}", s.requireAdmin(s.handleAdminCacheDelete))
		mux.HandleFunc("GET /nb/admin/api/console", s.requireAdmin(s.handleAdminConsole))
		mux.HandleFunc("POST /nb/admin/api/restart", s.requireAdmin(s.handleAdminRestart))
		// 个人中心（用户自助，身份经 ?nb_user=）
		mux.HandleFunc("GET /nb/me/", s.handleMePage)
		mux.HandleFunc("GET /nb/me/api/profile", s.handleMeProfile)
		mux.HandleFunc("POST /nb/me/api/cookie", s.handleMeCookie)
		mux.HandleFunc("POST /nb/me/api/mode", s.handleMeMode)
	}
	mux.Handle("/", s.proxy)
	return mux
}

// ReloadPathMaps 从 DB 重建路径映射规则（DB 有映射时优先，否则回退
// NB_PATH_MAP 环境变量解析），并把指纹解析器换成 DynamicMapper 实现热更新。
// pathmaps 的 POST/DELETE 后调用。
func (s *Server) ReloadPathMaps() {
	rules := resolvePathRules(s)
	s.pathMu.Lock()
	s.pathRules = rules
	s.pathMu.Unlock()
	if lr, ok := s.finger.(*LiveFingerprintResolver); ok && lr != nil {
		lr.SetMapper(NewDynamicMapper(s.pathRulesSnapshot))
	}
}

// pathRulesSnapshot 返回规则的拷贝（DynamicMapper 每次调用取最新）。
func (s *Server) pathRulesSnapshot() []PathRule {
	s.pathMu.RLock()
	defer s.pathMu.RUnlock()
	out := make([]PathRule, len(s.pathRules))
	copy(out, s.pathRules)
	return out
}

// resolvePathRules：DB 的 path_maps 非空则用 DB（sub_path 空→"/"，按
// EmbyPrefix 长短排序沿用 ParsePathMap 逻辑），否则回退 NB_PATH_MAP 解析。
func resolvePathRules(s *Server) []PathRule {
	if s.db != nil {
		if ms, err := s.db.ListPathMaps(context.Background()); err == nil && len(ms) > 0 {
			rules := make([]PathRule, 0, len(ms))
			for _, m := range ms {
				p115 := m.SubPath
				if p115 == "" {
					p115 = "/"
				}
				rules = append(rules, PathRule{
					EmbyPrefix: strings.TrimSuffix(m.EmbyPath, "/"),
					Path115:    strings.TrimSuffix(p115, "/"),
				})
			}
			sort.Slice(rules, func(i, j int) bool {
				return len(rules[i].EmbyPrefix) > len(rules[j].EmbyPrefix)
			})
			return rules
		} else if err != nil {
			s.logger.Printf("ReloadPathMaps 读 DB 失败，回退 NB_PATH_MAP: %v", err)
		}
	}
	rules, err := ParsePathMap(s.cfg.PathMap)
	if err != nil {
		s.logger.Printf("ReloadPathMaps NB_PATH_MAP 解析失败: %v", err)
		return nil
	}
	return rules
}
