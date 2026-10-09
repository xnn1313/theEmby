package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nextemby-replay/engine"
)

// webUIState 是 /nb/admin 与 /nb/me 的共享状态。
type webUIState struct {
	adminToken string
	cookies    CookieStore
	// validateCookie 对 115 Cookie 做 LoginCheck，返回可用的 DriveClient；
	// nil 表示跳过校验（测试用）。
	validateCookie func(ctx context.Context, userID, cookie string) (engine.DriveClient, error)

	mu          sync.Mutex
	userClients map[string]engine.DriveClient // userID -> 自有 115 client（懒加载缓存）

	sessMu   sync.Mutex
	sessions map[string]time.Time // 登录 session token -> 过期时间（24h）
}

// EnableWebUI 启用管理后台与个人中心（重复调用会覆盖旧配置）。
func (s *Server) EnableWebUI(adminToken string, cookies CookieStore, validateCookie func(ctx context.Context, userID, cookie string) (engine.DriveClient, error)) {
	s.webUI = &webUIState{
		adminToken:     adminToken,
		cookies:        cookies,
		validateCookie: validateCookie,
		userClients:    map[string]engine.DriveClient{},
		sessions:       map[string]time.Time{},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- 总览

type poolStatusJSON struct {
	ID          string `json:"id"`
	Healthy     bool   `json:"healthy"`
	LockedUsers int    `json:"lockedUsers"`
	MaxUsers    int    `json:"maxUsers"`
}

func (s *Server) poolStatus(ctx context.Context) ([]poolStatusJSON, error) {
	accounts, err := s.engine.PoolAccounts(ctx)
	if err != nil {
		return nil, err
	}
	locked := map[string]int{}
	for _, l := range s.engine.PoolLocks() {
		locked[l.AccountID]++
	}
	out := make([]poolStatusJSON, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, poolStatusJSON{
			ID:          a.ID,
			Healthy:     a.Healthy,
			LockedUsers: locked[a.ID],
			MaxUsers:    a.MaxUsersOrDefault(),
		})
	}
	return out, nil
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	branches := map[string]int{}
	total := 0
	if s.db != nil {
		// DB 优先：decisions 表全量聚合（不受 200 条 ring buffer 限制）。
		var err error
		branches, err = s.db.BranchStats(r.Context(), midnight.Unix())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, n := range branches {
			total += n
		}
	} else {
		for _, d := range s.engine.RecentDecisions(200) {
			if d.At.Before(midnight) {
				continue
			}
			branches[d.Branch]++
			total++
		}
	}
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(branches[engine.BranchCacheHit]) / float64(total)
	}
	pool, err := s.poolStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"branches":     branches,
		"totalToday":   total,
		"cacheHitRate": hitRate,
		"pool":         pool,
	})
}

// ---------------------------------------------------------------- 用户管理

type adminUserJSON struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	Template  string `json:"template"`
	Sessions  int    `json:"sessions"`
	HasCookie bool   `json:"hasCookie"`
	OK        int64  `json:"ok"`
	Fail      int64  `json:"fail"`
	ExpiresAt int64  `json:"expiresAt"`
	Banned    bool   `json:"banned"`
	Remark    string `json:"remark"`
	Plays30d  int    `json:"plays30d"`
	LastUA    string `json:"lastUA"`
	// Drive：115 模式显示"自备网盘"，池模式显示"负载均衡"。
	Drive string `json:"drive"`
	// DriveOk：池模式恒 true；115 模式 = 用户是否已绑 Cookie。
	DriveOk bool `json:"driveOk"`
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.engine.Users().(engine.UserLister)
	if !ok {
		http.Error(w, "user listing not implemented by user store (待接 DB)", http.StatusNotImplemented)
		return
	}
	users, err := lister.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats := s.engine.AllUserStats()
	if s.db != nil {
		// DB 优先：从 decisions 表聚合（全量，不受内存计数重启丢失影响）。
		if ds, err := s.db.UserStats(r.Context()); err == nil {
			stats = ds
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	out := make([]adminUserJSON, 0, len(users))
	for _, u := range users {
		st := stats[u.ID]
		drive, driveOk := "负载均衡", true
		if u.Mode == engine.ModeOwn115 {
			drive = "自备网盘"
			driveOk = s.webUI.cookies.HasCookie(r.Context(), u.ID)
		}
		var plays30d int
		var lastUA string
		if s.db != nil {
			if p, ua, err := s.db.UserActivity(r.Context(), u.ID); err == nil {
				plays30d, lastUA = p, ua
			}
		}
		out = append(out, adminUserJSON{
			ID:        u.ID,
			Mode:      u.Mode,
			Template:  u.Template,
			Sessions:  s.engine.Sessions(u.ID),
			HasCookie: s.webUI.cookies.HasCookie(r.Context(), u.ID),
			OK:        st.OK,
			Fail:      st.Fail,
			ExpiresAt: u.ExpiresAt,
			Banned:    u.Banned,
			Remark:    u.Remark,
			Plays30d:  plays30d,
			LastUA:    lastUA,
			Drive:     drive,
			DriveOk:   driveOk,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Mode      string  `json:"mode"`
		Template  *string `json:"template"`
		ExpiresAt *int64  `json:"expiresAt"`
		Banned    *bool   `json:"banned"`
		Remark    *string `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Mode != "" && body.Mode != engine.ModeOwn115 && body.Mode != engine.ModePool {
		http.Error(w, "mode must be 115 or pool", http.StatusBadRequest)
		return
	}
	updater, ok := s.engine.Users().(engine.UserUpdater)
	if !ok {
		http.Error(w, "user update not implemented by user store (待接 DB)", http.StatusNotImplemented)
		return
	}
	u, err := s.engine.Users().GetUser(r.Context(), id)
	if err != nil {
		http.Error(w, "unknown user", http.StatusNotFound)
		return
	}
	// 只更新提供的字段（指针字段区分"不传"与"置空/零值"）。
	if body.Mode != "" {
		u.Mode = body.Mode
	}
	if body.Template != nil {
		u.Template = *body.Template
	}
	if body.ExpiresAt != nil {
		u.ExpiresAt = *body.ExpiresAt
	}
	if body.Banned != nil {
		u.Banned = *body.Banned
	}
	if body.Remark != nil {
		u.Remark = *body.Remark
	}
	if err := updater.UpdateUser(r.Context(), u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 池账号

func (s *Server) handleAdminPool(w http.ResponseWriter, r *http.Request) {
	pool, err := s.poolStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pool)
}

func (s *Server) handleAdminPoolHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.engine.SetPoolAccountHealthy(r.Context(), id, body.Healthy); err != nil {
		if strings.Contains(err.Error(), "unknown") {
			http.Error(w, "unknown pool account", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 池账号健康变更 id=%s healthy=%v", id, body.Healthy)
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 播放日志

type decisionJSON struct {
	At         string `json:"at"`
	UserID     string `json:"userID"`
	Branch     string `json:"branch"`
	Allowed    bool   `json:"allowed"`
	DenyReason string `json:"denyReason,omitempty"`
	AccountID  string `json:"accountID"`
	ElapsedMs  int64  `json:"elapsedMs"`
}

func toDecisionJSON(d engine.DecisionSummary) decisionJSON {
	return decisionJSON{
		At:         d.At.Format("2006-01-02 15:04:05"),
		UserID:     d.UserID,
		Branch:     d.Branch,
		Allowed:    d.Allowed,
		DenyReason: d.DenyReason,
		AccountID:  d.AccountID,
		ElapsedMs:  d.ElapsedMs(),
	}
}

func (s *Server) handleAdminDecisions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	decs := s.engine.RecentDecisions(limit)
	if s.db != nil {
		// DB 优先：decisions 表（全量历史）。
		var err error
		decs, err = s.db.RecentDecisions(r.Context(), limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	out := make([]decisionJSON, 0, len(decs))
	for _, d := range decs {
		out = append(out, toDecisionJSON(d))
	}
	writeJSON(w, out)
}

// ---------------------------------------------------------------- 配置查看

// 注意：API Key / Cookie / token 永不经此接口输出。
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"upstream": s.cfg.Upstream,
		"listen":   s.cfg.Listen,
		"pathMap":  s.cfg.PathMap,
	})
}

// ---------------------------------------------------------------- 页面

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mustReadWebFile("web/admin.html"))
}

func mustReadWebFile(name string) []byte {
	b, err := webFS.ReadFile(name)
	if err != nil {
		panic("gateway: 内嵌页面缺失 " + name + ": " + err.Error())
	}
	return b
}
