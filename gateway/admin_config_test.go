package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nextemby-replay/engine"
	emem "nextemby-replay/engine/memory"
	"nextemby-replay/store"
)

// cfgFixture 是管理后台可配置 API 测试的装配：真实 SQLite + 上游 recorder +
// fake 115 fetcher + srv 句柄（AttachStore/AttachLogSink/ReloadPathMaps 已接）。
type cfgFixture struct {
	gw  *httptest.Server
	srv *Server
	st  *store.Store
	eng *engine.Engine
	rec *upstreamRecorder
}

func newCfgFixture(t *testing.T) *cfgFixture {
	t.Helper()
	ctx := context.Background()

	rec := &upstreamRecorder{}
	up := httptest.NewServer(rec)
	t.Cleanup(up.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().EnsureAccount(ctx, "115小1", 4); err != nil {
		t.Fatal(err)
	}

	d1 := emem.NewDriveClient("115小1")
	seed := emem.NewDriveClient("115大")
	eng := engine.New(
		engine.Config{Templates: map[string]int{"vip": 5}, DefaultLimit: 1},
		engine.Deps{
			Users:   st.Users(),
			Pool:    st.Pool(),
			Seed:    seed,
			PoolDrv: map[string]engine.DriveClient{"115小1": d1},
			Shield:  emem.NewShieldClient(map[string]engine.ShieldResult{}),
			Records: st.Records(),
		},
	)
	eng.OnDecision = func(s engine.DecisionSummary) {
		if err := st.LogDecision(s); err != nil {
			t.Errorf("log decision: %v", err)
		}
	}

	logBuf := &bytes.Buffer{}
	logger := log.New(logBuf, "[test] ", 0)
	cfg := Config{Upstream: up.URL, APIKey: testAPIKey, HMACKey: []byte(testHMACKey),
		Listen: ":8091", PathMap: "/emby/media=/115/媒体库"}
	fetcher := &fakeFetcher{}
	fetcher.seed("/115/媒体库/去有风的地方.S01E04.mp4", testSHA1)
	finger := NewLiveFingerprintResolver(NewPathMapper(nil), fetcher)
	srv, err := NewServer(cfg, eng,
		map[string]engine.DriveClient{"115大": seed, "115小1": d1},
		finger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := st.Cookies(testCookieKey)
	if err != nil {
		t.Fatal(err)
	}
	validate := func(_ context.Context, userID, cookie string) (engine.DriveClient, error) {
		if cookie == "bad-cookie" {
			return nil, errors.New("115 login check failed")
		}
		return emem.NewDriveClient("user:" + userID), nil
	}
	srv.EnableWebUI("test-admin-token", cookies, validate)
	srv.AttachStore(st)
	srv.AttachLogSink(st)
	srv.ReloadPathMaps()

	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	return &cfgFixture{gw: gw, srv: srv, st: st, eng: eng, rec: rec}
}

// adminReq 发一个带 ?token= 鉴权的管理后台请求。
func adminReq(t *testing.T, fx *cfgFixture, method, rawPath string, body any) *http.Response {
	t.Helper()
	u := fx.gw.URL + rawPath
	sep := "?"
	if strings.Contains(rawPath, "?") {
		sep = "&"
	}
	u += sep + "token=test-admin-token"
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func adminBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newJarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func postJSON(t *testing.T, client *http.Client, url string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest("POST", url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func cfgPostPlayback(t *testing.T, fx *cfgFixture, nbUser string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", fx.gw.URL+"/emby/Items/x/PlaybackInfo?nb_user="+nbUser,
		strings.NewReader(`{"UserId":"u"}`))
	req.Header.Set("User-Agent", "TestPlayer/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ---------------------------------------------------------------- settings

func TestAdminSettingsCRUD(t *testing.T) {
	fx := newCfgFixture(t)
	ctx := context.Background()

	resp := adminReq(t, fx, "PUT", "/nb/admin/api/settings", map[string]any{
		"emby.addr": "10.0.0.5", "nextfind.agent_key": "AK-super-secret",
		"num": 42, "flag": true,
	})
	body := adminBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, body)
	}

	var m map[string]string
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/settings", nil), &m)
	if m["emby.addr"] != "10.0.0.5" || m["num"] != "42" || m["flag"] != "true" {
		t.Fatalf("settings = %v", m)
	}
	if m["nextfind.agent_key"] != "••••••••" {
		t.Fatalf("agent_key not masked: %q", m["nextfind.agent_key"])
	}
	if m["admin.cookie_bound"] != "0" {
		t.Fatalf("cookie_bound = %q", m["admin.cookie_bound"])
	}
	if _, has := m["admin.password_hash"]; has {
		t.Fatal("password_hash must never be returned")
	}
	for k, v := range m {
		if strings.Contains(v, "AK-super-secret") {
			t.Fatalf("secret leaked in %s", k)
		}
	}

	// prefix 过滤
	var pm map[string]string
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/settings?prefix=emby.", nil), &pm)
	if pm["emby.addr"] != "10.0.0.5" {
		t.Fatalf("prefix = %v", pm)
	}
	if _, has := pm["nextfind.agent_key"]; has {
		t.Fatalf("prefix leaked: %v", pm)
	}

	// 拒绝写 password_hash
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/settings", map[string]string{
		"admin.password_hash": "v1$fake$fake",
	})
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 直接在 DB 里种一个 hash，GET 仍不返回
	if _, err := hashPassword("pw123"); err != nil {
		t.Fatal(err)
	}
	h, _ := hashPassword("pw123")
	if err := fx.st.SetSetting(ctx, "admin.password_hash", h); err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/settings", nil), &m)
	if _, has := m["admin.password_hash"]; has {
		t.Fatal("password_hash must never be returned")
	}
}

// ---------------------------------------------------------------- accounts

func findAccount(t *testing.T, fx *cfgFixture, id string) accountJSON {
	t.Helper()
	var accs []accountJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/accounts", nil), &accs)
	for _, a := range accs {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %s not found in %+v", id, accs)
	return accountJSON{}
}

func TestAdminAccountsCRUD(t *testing.T) {
	fx := newCfgFixture(t)

	// 种子账号：115大(seed)、115小1、115小2(pool)
	var accs []accountJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/accounts", nil), &accs)
	if len(accs) != 3 {
		t.Fatalf("accounts = %+v", accs)
	}
	a := findAccount(t, fx, "115大")
	if a.Kind != "seed" || a.HasCookie || a.CookieMask != "未绑定" || a.LockedUsers != 0 {
		t.Fatalf("115大 = %+v", a)
	}

	bad := []struct {
		name string
		body map[string]any
	}{
		{"no id", map[string]any{"kind": "pool"}},
		{"bad kind", map[string]any{"id": "x", "kind": "s3"}},
		{"bad cookie", map[string]any{"id": "115小3", "kind": "pool", "cookie": "bad-cookie"}},
	}
	for _, tc := range bad {
		resp := adminReq(t, fx, "POST", "/nb/admin/api/accounts", tc.body)
		if resp.StatusCode != 400 {
			resp.Body.Close()
			t.Fatalf("%s: want 400, got %d", tc.name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// 新增成功
	resp := adminReq(t, fx, "POST", "/nb/admin/api/accounts", map[string]any{
		"id": "115小3", "name": "小3", "kind": "pool", "cookie": "good-cookie",
		"uid": "U123", "maxUsers": 3, "rapidDir": "/recv",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	a = findAccount(t, fx, "115小3")
	if !a.HasCookie || a.CookieMask != "UID:U123 | ••••••••" || a.UID != "U123" ||
		a.MaxUsers != 3 || a.RapidDir != "/recv" || !a.Healthy || !a.Enabled {
		t.Fatalf("115小3 = %+v", a)
	}
	// GET 永不回显 cookie 明文
	raw := adminBody(t, adminReq(t, fx, "GET", "/nb/admin/api/accounts", nil))
	if strings.Contains(raw, "good-cookie") {
		t.Fatal("cookie plaintext leaked in accounts list")
	}

	// PUT patch：只改 maxUsers，其他保留
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/accounts/115小3", map[string]any{"maxUsers": 5})
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	a = findAccount(t, fx, "115小3")
	if a.MaxUsers != 5 || a.Name != "小3" || !a.HasCookie {
		t.Fatalf("patch changed unexpected fields: %+v", a)
	}
	// PUT 非法 kind → 400；坏 cookie → 400；未知账号 → 404
	for _, tc := range []struct {
		path string
		body map[string]any
		want int
	}{
		{"/nb/admin/api/accounts/115小3", map[string]any{"kind": "s3"}, 400},
		{"/nb/admin/api/accounts/115小3", map[string]any{"cookie": "bad-cookie"}, 400},
		{"/nb/admin/api/accounts/nope", map[string]any{"maxUsers": 1}, 404},
	} {
		resp := adminReq(t, fx, "PUT", tc.path, tc.body)
		if resp.StatusCode != tc.want {
			resp.Body.Close()
			t.Fatalf("put %s: want %d, got %d", tc.path, tc.want, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// PUT 更新 cookie（校验通过），GET 仍不回显明文
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/accounts/115小3", map[string]any{"cookie": "good-cookie-2"})
	if resp.StatusCode != 200 {
		t.Fatalf("put cookie: %d", resp.StatusCode)
	}
	resp.Body.Close()
	raw = adminBody(t, adminReq(t, fx, "GET", "/nb/admin/api/accounts", nil))
	if strings.Contains(raw, "good-cookie-2") {
		t.Fatal("cookie plaintext leaked after update")
	}

	// DELETE
	resp = adminReq(t, fx, "DELETE", "/nb/admin/api/accounts/115小3", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp.Body.Close()
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/accounts", nil), &accs)
	for _, x := range accs {
		if x.ID == "115小3" {
			t.Fatalf("still present: %+v", accs)
		}
	}
}

// ---------------------------------------------------------------- pathmaps

func TestAdminPathMapsCRUD(t *testing.T) {
	fx := newCfgFixture(t)
	ctx := context.Background()

	// 种子映射
	var ms []pathMapJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/pathmaps", nil), &ms)
	if len(ms) != 1 || ms[0].EmbyPath != "/CloudNAS/CloudDrive/115open" {
		t.Fatalf("pathmaps = %+v", ms)
	}

	// accountId 不存在 → 400
	resp := adminReq(t, fx, "POST", "/nb/admin/api/pathmaps", map[string]string{
		"embyPath": "/x", "accountId": "nope",
	})
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// embyPath 空 → 400
	resp = adminReq(t, fx, "POST", "/nb/admin/api/pathmaps", map[string]string{
		"embyPath": "", "accountId": "115小1",
	})
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 新增
	resp = adminReq(t, fx, "POST", "/nb/admin/api/pathmaps", map[string]string{
		"embyPath": "/emby/media", "accountId": "115小1", "subPath": "/115/媒体库",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/pathmaps", nil), &ms)
	if len(ms) != 2 {
		t.Fatalf("pathmaps = %+v", ms)
	}

	// ReloadPathMaps 生效：resolver 能解析新规则
	sha, ok := fx.srv.finger.ResolveSHA1(ctx, "/emby/media/去有风的地方.S01E04.mp4")
	if !ok || sha != testSHA1 {
		t.Fatalf("resolve after reload: ok=%v sha=%q", ok, sha)
	}

	// DELETE（embyPath 需 URL 编码）
	resp = adminReq(t, fx, "DELETE", "/nb/admin/api/pathmaps/%2Femby%2Fmedia", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/pathmaps", nil), &ms)
	if len(ms) != 1 {
		t.Fatalf("pathmaps = %+v", ms)
	}
	if _, ok := fx.srv.finger.ResolveSHA1(ctx, "/emby/media/去有风的地方.S01E04.mp4"); ok {
		t.Fatal("deleted rule still resolves")
	}
}

// ---------------------------------------------------------------- templates

func TestAdminTemplatesCRUD(t *testing.T) {
	fx := newCfgFixture(t)

	var ts []templateJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/templates", nil), &ts)
	if len(ts) != 1 || ts[0].Name != "vip" {
		t.Fatalf("templates = %+v", ts)
	}

	// POST 无 name → 400
	resp := adminReq(t, fx, "POST", "/nb/admin/api/templates", map[string]any{"maxConcurrent": 2})
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// POST
	resp = adminReq(t, fx, "POST", "/nb/admin/api/templates", map[string]any{
		"name": "t1", "maxConcurrent": 2, "dailyPlays": 5,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/templates", nil), &ts)
	if len(ts) != 2 {
		t.Fatalf("templates = %+v", ts)
	}

	// PUT（以路径 name 为准）
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/templates/t1", map[string]any{
		"name": "t1", "maxConcurrent": 9, "dailyPlays": 5,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	resp.Body.Close()
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/templates", nil), &ts)
	for _, x := range ts {
		if x.Name == "t1" && x.MaxConcurrent != 9 {
			t.Fatalf("t1 = %+v", x)
		}
	}

	// DELETE t1
	resp = adminReq(t, fx, "DELETE", "/nb/admin/api/templates/t1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 不允许删除最后一个模板
	resp = adminReq(t, fx, "DELETE", "/nb/admin/api/templates/vip", nil)
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/templates", nil), &ts)
	if len(ts) != 1 {
		t.Fatalf("templates = %+v", ts)
	}
}

// ---------------------------------------------------------------- users

func findAdminUser(t *testing.T, fx *cfgFixture, id string) adminUserJSON {
	t.Helper()
	var users []adminUserJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/users", nil), &users)
	for _, u := range users {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("user %s not found in %+v", id, users)
	return adminUserJSON{}
}

func TestAdminUsersCreateUpdateDelete(t *testing.T) {
	fx := newCfgFixture(t)

	// POST
	resp := adminReq(t, fx, "POST", "/nb/admin/api/users", map[string]any{
		"id": "u1", "mode": "pool", "template": "vip", "remark": "测试",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	// 重复 → 409
	resp = adminReq(t, fx, "POST", "/nb/admin/api/users", map[string]any{"id": "u1"})
	if resp.StatusCode != 409 {
		resp.Body.Close()
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 非法 mode → 400；无 id → 400
	for _, b := range []map[string]any{{"id": "u2", "mode": "s3"}, {"mode": "pool"}} {
		resp := adminReq(t, fx, "POST", "/nb/admin/api/users", b)
		if resp.StatusCode != 400 {
			resp.Body.Close()
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	u := findAdminUser(t, fx, "u1")
	if u.Mode != "pool" || u.Template != "vip" || u.Remark != "测试" ||
		u.Banned || u.ExpiresAt != 0 || u.Drive != "负载均衡" || !u.DriveOk {
		t.Fatalf("u1 = %+v", u)
	}

	// PUT：ban + expiresAt
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/users/u1", map[string]any{
		"banned": true, "expiresAt": 999,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, adminBody(t, resp))
	}
	u = findAdminUser(t, fx, "u1")
	if !u.Banned || u.ExpiresAt != 999 || u.Mode != "pool" || u.Template != "vip" {
		t.Fatalf("u1 after put = %+v", u)
	}
	// PUT 部分字段：mode 不变
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/users/u1", map[string]any{
		"remark": "r2", "banned": false, "mode": "115",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	resp.Body.Close()
	u = findAdminUser(t, fx, "u1")
	if u.Banned || u.Remark != "r2" || u.Mode != "115" || u.Drive != "自备网盘" || u.DriveOk {
		t.Fatalf("u1 after put2 = %+v", u)
	}

	// DELETE
	resp = adminReq(t, fx, "DELETE", "/nb/admin/api/users/u1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp.Body.Close()
	var users []adminUserJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/users", nil), &users)
	for _, x := range users {
		if x.ID == "u1" {
			t.Fatalf("still present: %+v", users)
		}
	}
}

// ---------------------------------------------------------------- 播放前置检查

func TestPlaybackPrecheck(t *testing.T) {
	fx := newCfgFixture(t)

	// 建映射让指纹命中（否则优雅降级透传，走不到引擎/前置检查后逻辑）。
	resp := adminReq(t, fx, "POST", "/nb/admin/api/pathmaps", map[string]string{
		"embyPath": "/emby/media", "accountId": "115小1", "subPath": "/115/媒体库",
	})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pathmap: %d", resp.StatusCode)
	}

	mkUser := func(id string) {
		t.Helper()
		resp := adminReq(t, fx, "POST", "/nb/admin/api/users", map[string]any{
			"id": id, "mode": "pool", "template": "vip",
		})
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("create %s: %d", id, resp.StatusCode)
		}
	}
	mkUser("banned1")
	mkUser("exp1")

	resp = adminReq(t, fx, "PUT", "/nb/admin/api/users/banned1", map[string]any{"banned": true})
	resp.Body.Close()
	past := time.Now().Add(-time.Hour).Unix()
	resp = adminReq(t, fx, "PUT", "/nb/admin/api/users/exp1", map[string]any{"expiresAt": past})
	resp.Body.Close()

	// banned → 403 user_banned
	resp = cfgPostPlayback(t, fx, "banned1")
	var errBody map[string]string
	decodeJSON(t, resp, &errBody)
	if resp.StatusCode != 403 || errBody["error"] != "user_banned" {
		t.Fatalf("banned: %d %v", resp.StatusCode, errBody)
	}
	// expired → 403 user_expired
	resp = cfgPostPlayback(t, fx, "exp1")
	decodeJSON(t, resp, &errBody)
	if resp.StatusCode != 403 || errBody["error"] != "user_expired" {
		t.Fatalf("expired: %d %v", resp.StatusCode, errBody)
	}
	// 正常用户能播（vip 模板 daily_plays=3，首次不触发）
	resp = cfgPostPlayback(t, fx, "lzy")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("lzy: %d", resp.StatusCode)
	}

	// user 日志落库（sink 异步，给一点时间）
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for {
		logs, _ := fx.st.ListLogs(ctx, "user", 10)
		hit := 0
		for _, l := range logs {
			if strings.Contains(l.Message, "banned1") {
				hit++
			}
		}
		if hit > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("banned user log not persisted")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPlaybackDailyLimit(t *testing.T) {
	fx := newCfgFixture(t)
	resp := adminReq(t, fx, "POST", "/nb/admin/api/pathmaps", map[string]string{
		"embyPath": "/emby/media", "accountId": "115小1", "subPath": "/115/媒体库",
	})
	resp.Body.Close()
	resp = adminReq(t, fx, "POST", "/nb/admin/api/users", map[string]any{
		"id": "dl1", "mode": "pool", "template": "vip", // vip: daily_plays=3
	})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	for i := 0; i < 3; i++ {
		resp := cfgPostPlayback(t, fx, "dl1")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("play %d: %d", i, resp.StatusCode)
		}
	}
	resp = cfgPostPlayback(t, fx, "dl1")
	var errBody map[string]string
	decodeJSON(t, resp, &errBody)
	if resp.StatusCode != 429 || errBody["error"] != "daily_limit" {
		t.Fatalf("4th play: %d %v", resp.StatusCode, errBody)
	}

	// UA 透传进决策落库
	ctx := context.Background()
	decs, err := fx.st.RecentDecisions(ctx, 1)
	if err != nil || len(decs) != 1 || decs[0].UA != "TestPlayer/1.0" {
		t.Fatalf("ua not persisted: %+v err=%v", decs, err)
	}
}
