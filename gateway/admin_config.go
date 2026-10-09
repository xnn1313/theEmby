package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nextemby-replay/engine"
	"nextemby-replay/store"
)

// admin_config.go：管理后台全部可配置 API（settings / accounts115 / pathmaps /
// templates / users 增删 / logs / cache / console / restart）。
// 全部经 requireAdmin 鉴权。Cookie / 密码 hash / API Key 永不返回、永不打日志。

// requireStore 未接 DB 时 500（这些接口都依赖 SQLite）。
func (s *Server) requireStore(w http.ResponseWriter) bool {
	if s.db == nil {
		http.Error(w, "store not attached", http.StatusInternalServerError)
		return false
	}
	return true
}

// ---------------------------------------------------------------- settings

// maskedSecret 是敏感配置项的展示掩码。
const maskedSecret = "••••••••"

func (s *Server) accounts115Store() (*store.Accounts115Store, error) {
	return s.db.Accounts115(s.cfg.CookieKey)
}

// handleAdminSettings GET /nb/admin/api/settings?prefix=。
// admin.password_hash 永不返回；nextfind.agent_key 掩码展示；
// 额外加算 admin.cookie_bound（"1"/"0"）= 是否有 kind=seed 且已绑 cookie 的账号。
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	m, err := s.db.SettingsByPrefix(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	delete(m, passwordHashSettingKey)
	if _, ok := m["nextfind.agent_key"]; ok {
		m["nextfind.agent_key"] = maskedSecret
	}
	bound := "0"
	if as, err := s.accounts115Store(); err == nil {
		if accs, err := as.ListAccounts115(r.Context()); err == nil {
			for _, a := range accs {
				if a.Kind == "seed" && a.HasCookie {
					bound = "1"
					break
				}
			}
		}
	}
	m["admin.cookie_bound"] = bound
	writeJSON(w, m)
}

// handleAdminSettingsPut PUT /nb/admin/api/settings {k:v}：逐个 SetSetting。
// 拒绝写 admin.password_hash（改密码走 login API）。
func (s *Server) handleAdminSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, v := range body {
		if k == "" {
			http.Error(w, "empty key", http.StatusBadRequest)
			return
		}
		if k == passwordHashSettingKey {
			writeJSONStatus(w, http.StatusBadRequest,
				map[string]string{"error": "password_hash is read-only, use the login API"})
			return
		}
		var sv string
		switch t := v.(type) {
		case string:
			sv = t
		case float64:
			sv = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			sv = strconv.FormatBool(t)
		case nil:
			sv = ""
		default:
			b, _ := json.Marshal(t)
			sv = string(b)
		}
		if err := s.db.SetSetting(r.Context(), k, sv); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- accounts115

type accountJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	HasCookie   bool   `json:"hasCookie"`
	CookieMask  string `json:"cookieMask"`
	UID         string `json:"uid"`
	MaxUsers    int    `json:"maxUsers"`
	LockedUsers int    `json:"lockedUsers"`
	Healthy     bool   `json:"healthy"`
	Enabled     bool   `json:"enabled"`
	RapidDir    string `json:"rapidDir"`
	Quota       string `json:"quota"`
}

func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	as, err := s.accounts115Store()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	accs, err := as.ListAccounts115(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	locked := map[string]int{}
	for _, l := range s.engine.PoolLocks() {
		locked[l.AccountID]++
	}
	out := make([]accountJSON, 0, len(accs))
	for _, a := range accs {
		out = append(out, accountJSON{
			ID:          a.ID,
			Name:        a.Name,
			Kind:        a.Kind,
			HasCookie:   a.HasCookie,
			CookieMask:  store.CookieMask(a),
			UID:         a.UID,
			MaxUsers:    a.MaxUsers,
			LockedUsers: locked[a.ID],
			Healthy:     a.Healthy,
			Enabled:     a.Enabled,
			RapidDir:    a.RapidDir,
			Quota:       a.QuotaInfo,
		})
	}
	writeJSON(w, out)
}

// checkAccountCookie 用 webUI.validateCookie 做 LoginCheck；
// validateCookie 为 nil（测试）时跳过校验。
func (s *Server) checkAccountCookie(ctx context.Context, id, cookie string) error {
	if s.webUI == nil || s.webUI.validateCookie == nil {
		return nil
	}
	_, err := s.webUI.validateCookie(ctx, id, cookie)
	return err
}

func validAccountKind(k string) bool { return k == "seed" || k == "pool" }

func (s *Server) handleAdminAccountCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	var body struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Cookie   string `json:"cookie"`
		UID      string `json:"uid"`
		MaxUsers int    `json:"maxUsers"`
		Enabled  *bool  `json:"enabled"`
		RapidDir string `json:"rapidDir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if !validAccountKind(body.Kind) {
		http.Error(w, "kind must be seed or pool", http.StatusBadRequest)
		return
	}
	if body.Cookie != "" {
		if err := s.checkAccountCookie(r.Context(), body.ID, body.Cookie); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "cookie_invalid"})
			return
		}
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	as, err := s.accounts115Store()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := body.Name
	if name == "" {
		name = body.ID
	}
	if err := as.UpsertAccount115(r.Context(), store.Account115Input{
		ID: body.ID, Name: name, Kind: body.Kind, Cookie: body.Cookie,
		UID: body.UID, MaxUsers: body.MaxUsers, Healthy: true,
		Enabled: enabled, RapidDir: body.RapidDir,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 只打 id/kind，cookie 明文永不打日志。
	s.logger.Printf("admin 新增115账号 id=%s kind=%s", body.ID, body.Kind)
	writeJSON(w, map[string]any{"ok": true})
}

// handleAdminAccountUpdate PUT /nb/admin/api/accounts/{id}：patch 语义，
// 只更新提供的字段；cookie 非空先 LoginCheck 再存（不提供则保留旧 cookie）。
func (s *Server) handleAdminAccountUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	as, err := s.accounts115Store()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cur, err := as.GetAccount115(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "unknown") {
			http.Error(w, "unknown account", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Kind     *string `json:"kind"`
		Cookie   *string `json:"cookie"`
		UID      *string `json:"uid"`
		MaxUsers *int    `json:"maxUsers"`
		Healthy  *bool   `json:"healthy"`
		Enabled  *bool   `json:"enabled"`
		RapidDir *string `json:"rapidDir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	in := store.Account115Input{
		ID: id, Name: cur.Name, Kind: cur.Kind, UID: cur.UID,
		MaxUsers: cur.MaxUsers, Healthy: cur.Healthy,
		Enabled: cur.Enabled, RapidDir: cur.RapidDir,
	}
	if body.Name != nil {
		in.Name = *body.Name
	}
	if body.Kind != nil {
		if !validAccountKind(*body.Kind) {
			http.Error(w, "kind must be seed or pool", http.StatusBadRequest)
			return
		}
		in.Kind = *body.Kind
	}
	if body.Cookie != nil && *body.Cookie != "" {
		if err := s.checkAccountCookie(r.Context(), id, *body.Cookie); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "cookie_invalid"})
			return
		}
		in.Cookie = *body.Cookie
	}
	if body.UID != nil {
		in.UID = *body.UID
	}
	if body.MaxUsers != nil {
		in.MaxUsers = *body.MaxUsers
	}
	if body.Healthy != nil {
		in.Healthy = *body.Healthy
	}
	if body.Enabled != nil {
		in.Enabled = *body.Enabled
	}
	if body.RapidDir != nil {
		in.RapidDir = *body.RapidDir
	}
	if err := as.UpsertAccount115(r.Context(), in); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 更新115账号 id=%s", id)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	as, err := s.accounts115Store()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := as.DeleteAccount115(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 删除115账号 id=%s", id)
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- pathmaps

type pathMapJSON struct {
	EmbyPath  string `json:"embyPath"`
	AccountID string `json:"accountId"`
	SubPath   string `json:"subPath"`
}

func (s *Server) handleAdminPathMaps(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	ms, err := s.db.ListPathMaps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]pathMapJSON, 0, len(ms))
	for _, m := range ms {
		out = append(out, pathMapJSON{EmbyPath: m.EmbyPath, AccountID: m.AccountID, SubPath: m.SubPath})
	}
	writeJSON(w, out)
}

func (s *Server) handleAdminPathMapCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	var body struct {
		EmbyPath  string `json:"embyPath"`
		AccountID string `json:"accountId"`
		SubPath   string `json:"subPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.EmbyPath) == "" {
		http.Error(w, "embyPath is required", http.StatusBadRequest)
		return
	}
	// accountId 必须在 accounts115 中存在。
	as, err := s.accounts115Store()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := as.GetAccount115(r.Context(), body.AccountID); err != nil {
		http.Error(w, "unknown accountId", http.StatusBadRequest)
		return
	}
	if err := s.db.SetPathMap(r.Context(), body.EmbyPath, body.AccountID, body.SubPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.ReloadPathMaps()
	s.logger.Printf("admin 新增路径映射 %s -> %s", body.EmbyPath, body.AccountID)
	writeJSON(w, map[string]any{"ok": true})
}

// handleAdminPathMapDelete DELETE /nb/admin/api/pathmaps/{embyPath}。
// embyPath 需 URL 编码（encodeURIComponent），PathValue 取到的是解码值。
func (s *Server) handleAdminPathMapDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	embyPath := r.PathValue("embyPath")
	if err := s.db.DeletePathMap(r.Context(), embyPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.ReloadPathMaps()
	s.logger.Printf("admin 删除路径映射 %s", embyPath)
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- templates

type templateJSON struct {
	Name             string `json:"name"`
	MaxConcurrent    int    `json:"maxConcurrent"`
	MaxDevices       int    `json:"maxDevices"`
	DefaultLine      string `json:"defaultLine"`
	DailyPlays       int    `json:"dailyPlays"`
	UIDTaskLimit     int    `json:"uidTaskLimit"`
	LockHours        int    `json:"lockHours"`
	DailyRapid       int    `json:"dailyRapid"`
	GiftDays         int    `json:"giftDays"`
	ExpireDeleteDays int    `json:"expireDeleteDays"`
}

func toTemplateJSON(t store.Template) templateJSON {
	return templateJSON{
		Name: t.Name, MaxConcurrent: t.MaxConcurrent, MaxDevices: t.MaxDevices,
		DefaultLine: t.DefaultLine, DailyPlays: t.DailyPlays,
		UIDTaskLimit: t.UIDTaskLimit, LockHours: t.LockHours,
		DailyRapid: t.DailyRapid, GiftDays: t.GiftDays,
		ExpireDeleteDays: t.ExpireDeleteDays,
	}
}

func fromTemplateJSON(j templateJSON) store.Template {
	return store.Template{
		Name: j.Name, MaxConcurrent: j.MaxConcurrent, MaxDevices: j.MaxDevices,
		DefaultLine: j.DefaultLine, DailyPlays: j.DailyPlays,
		UIDTaskLimit: j.UIDTaskLimit, LockHours: j.LockHours,
		DailyRapid: j.DailyRapid, GiftDays: j.GiftDays,
		ExpireDeleteDays: j.ExpireDeleteDays,
	}
}

func (s *Server) handleAdminTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	ts, err := s.db.ListTemplates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]templateJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTemplateJSON(t))
	}
	writeJSON(w, out)
}

func (s *Server) handleAdminTemplateCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	var body templateJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := s.db.UpsertTemplate(r.Context(), fromTemplateJSON(body)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 新增模板 %s", body.Name)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdminTemplateUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	var body templateJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.Name = name // 以路径为准
	if err := s.db.UpsertTemplate(r.Context(), fromTemplateJSON(body)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 更新模板 %s", name)
	writeJSON(w, map[string]any{"ok": true})
}

// handleAdminTemplateDelete：不允许删除最后一个模板。
func (s *Server) handleAdminTemplateDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	ts, err := s.db.ListTemplates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(ts) <= 1 {
		writeJSONStatus(w, http.StatusBadRequest,
			map[string]string{"error": "cannot delete the last template"})
		return
	}
	if err := s.db.DeleteTemplate(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 删除模板 %s", name)
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- users 增删

type userDeleter interface {
	DeleteUser(context.Context, string) error
}

func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	var body struct {
		ID        string `json:"id"`
		Mode      string `json:"mode"`
		Template  string `json:"template"`
		ExpiresAt *int64 `json:"expiresAt"`
		Remark    string `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	mode := body.Mode
	if mode == "" {
		mode = engine.ModePool
	}
	if mode != engine.ModeOwn115 && mode != engine.ModePool {
		http.Error(w, "mode must be 115 or pool", http.StatusBadRequest)
		return
	}
	updater, ok := s.engine.Users().(engine.UserUpdater)
	if !ok {
		http.Error(w, "user update not implemented by user store", http.StatusNotImplemented)
		return
	}
	if _, err := s.engine.Users().GetUser(r.Context(), body.ID); err == nil {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "user_exists"})
		return
	}
	var expiresAt int64
	if body.ExpiresAt != nil {
		expiresAt = *body.ExpiresAt
	}
	if err := updater.UpdateUser(r.Context(), engine.User{
		ID: body.ID, Mode: mode, Template: body.Template,
		ExpiresAt: expiresAt, Remark: body.Remark,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 新增用户 id=%s mode=%s", body.ID, mode)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdminUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	deleter, ok := s.engine.Users().(userDeleter)
	if !ok {
		http.Error(w, "user delete not implemented by user store", http.StatusNotImplemented)
		return
	}
	if err := deleter.DeleteUser(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 删除用户 id=%s", id)
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- logs

type sysLogJSON struct {
	ID       int64  `json:"id"`
	Ts       int64  `json:"ts"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	logs, err := s.db.ListLogs(r.Context(), r.URL.Query().Get("category"), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]sysLogJSON, 0, len(logs))
	for _, l := range logs {
		out = append(out, sysLogJSON{ID: l.ID, Ts: l.Ts, Category: l.Category, Message: l.Message})
	}
	writeJSON(w, out)
}

func (s *Server) handleAdminLogsClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	if err := s.db.ClearLogs(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("admin 清空系统日志")
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- cache

func (s *Server) handleAdminCache(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.ListCache())
}

func (s *Server) handleAdminCacheClear(w http.ResponseWriter, r *http.Request) {
	s.ClearCache()
	s.logger.Printf("admin 清空直链缓存")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdminCacheDelete(w http.ResponseWriter, r *http.Request) {
	deleted := s.DeleteCache(r.PathValue("user"), r.PathValue("sha1"))
	writeJSON(w, map[string]any{"ok": deleted})
}

// ---------------------------------------------------------------- console

type uaStatJSON struct {
	Name    string `json:"name"`
	Plays   int64  `json:"plays"`
	Directs int64  `json:"directs"`
}

type timelineJSON struct {
	T     int64 `json:"t"`
	Plays int64 `json:"plays"`
}

// uaDisplayName 把原始 UA 归一到客户端名。
func uaDisplayName(ua string) string {
	switch {
	case strings.Contains(ua, "Filmly"):
		return "Filmly"
	case strings.Contains(ua, "VidHub"):
		return "VidHub"
	case strings.Contains(ua, "爆米花"):
		return "网易爆米花"
	case strings.Contains(ua, "Emby"):
		return "Emby"
	default:
		return "其他"
	}
}

var (
	transferBranches = []string{
		engine.BranchP2PRapid, engine.BranchSeedFallback,
		engine.BranchPoolShieldHit, engine.BranchPoolSeedFallback,
	}
	directBranches = []string{
		engine.BranchCacheHit, engine.BranchOwnDriveHit, engine.BranchPoolProbeHit,
	}
	cookieBranches = []string{ // 115 模式（用户自备盘）相关分支
		engine.BranchOwnDriveHit, engine.BranchP2PRapid, engine.BranchSeedFallback,
	}
)

func countBranches(agg store.RangeAgg, branches []string) int64 {
	set := map[string]bool{}
	for _, b := range branches {
		set[b] = true
	}
	var n int64
	for br, sp := range agg.ByBranch {
		if set[br] {
			n += sp.OK + sp.Fail
		}
	}
	return n
}

type cpuSample struct {
	total uint64
	idle  uint64
}

func readCPUStat() (cpuSample, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, x := range f[1:] {
			v, err := strconv.ParseUint(x, 10, 64)
			if err != nil {
				return cpuSample{}, err
			}
			vals = append(vals, v)
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait
		}
		return cpuSample{total: total, idle: idle}, nil
	}
	return cpuSample{}, os.ErrNotExist
}

// cpuUsagePercent 读 /proc/stat 两次（间隔 50ms）算 CPU 使用率；
// 非 Linux 或失败返回 0。
func cpuUsagePercent() float64 {
	a, err := readCPUStat()
	if err != nil {
		return 0
	}
	time.Sleep(50 * time.Millisecond)
	b, err := readCPUStat()
	if err != nil {
		return 0
	}
	dt := b.total - a.total
	di := b.idle - a.idle
	if dt == 0 || di > dt {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

func diskUsage() (used, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/data", &st); err != nil {
		if err := syscall.Statfs(".", &st); err != nil {
			return 0, 0
		}
	}
	total = st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if free > total {
		return 0, total
	}
	return total - free, total
}

// handleAdminConsole GET /nb/admin/api/console?range=24h|7d|30d：
// 播放聚合（分支归类/UA 归类/timeline）+ CPU/内存/磁盘。
func (s *Server) handleAdminConsole(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	rng := r.URL.Query().Get("range")
	now := time.Now()
	var since time.Time
	var bucket int64
	switch rng {
	case "7d":
		since, bucket = now.Add(-7*24*time.Hour), 21600
	case "30d":
		since, bucket = now.Add(-30*24*time.Hour), 86400
	default:
		rng, since, bucket = "24h", now.Add(-24*time.Hour), 3600
	}
	agg, err := s.db.DecisionRangeAgg(r.Context(), since.Unix(), bucket)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sourceRate := 0.0
	if agg.Total > 0 {
		sourceRate = float64(agg.OK) / float64(agg.Total)
	}

	merged := map[string]*uaStatJSON{}
	for _, u := range agg.ByUA {
		name := uaDisplayName(u.UA)
		m := merged[name]
		if m == nil {
			m = &uaStatJSON{Name: name}
			merged[name] = m
		}
		m.Plays += u.Plays
		m.Directs += u.Directs
	}
	uaStats := make([]uaStatJSON, 0, len(merged))
	for _, m := range merged {
		uaStats = append(uaStats, *m)
	}
	sort.Slice(uaStats, func(i, j int) bool { return uaStats[i].Plays > uaStats[j].Plays })

	timeline := make([]timelineJSON, 0, len(agg.Timeline))
	for _, p := range agg.Timeline {
		timeline = append(timeline, timelineJSON{T: p.T, Plays: p.Plays})
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	memMB := float64(int(float64(ms.Alloc)/1024/1024*10)) / 10
	diskUsed, diskTotal := diskUsage()

	writeJSON(w, map[string]any{
		"range":      rng,
		"total":      agg.Total,
		"transfer":   countBranches(agg, transferBranches),
		"direct":     countBranches(agg, directBranches),
		"cookie":     countBranches(agg, cookieBranches),
		"sourceRate": sourceRate,
		"cpu":        cpuUsagePercent(),
		"memMB":      memMB,
		"diskUsed":   diskUsed,
		"diskTotal":  diskTotal,
		"timeline":   timeline,
		"uaStats":    uaStats,
	})
}

// ---------------------------------------------------------------- restart

// handleAdminRestart POST /nb/admin/api/restart：
// 先回 {ok:true}，500ms 后进程退出（由 supervisor/docker 重启拉起）。
func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	s.logger.Printf("admin 请求重启服务")
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}
