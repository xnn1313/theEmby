package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"nextemby-replay/engine"
	"nextemby-replay/store"
)

// ---------------------------------------------------------------- 日志 sink

func TestLogSinkAsync(t *testing.T) {
	fx := newCfgFixture(t) // AttachLogSink 已接
	ctx := context.Background()

	fx.eng.LogFunc("play", "测试日志消息-xyz-123")
	deadline := time.Now().Add(3 * time.Second)
	for {
		logs, _ := fx.st.ListLogs(ctx, "play", 10)
		found := false
		for _, l := range logs {
			if strings.Contains(l.Message, "测试日志消息-xyz-123") {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("log not persisted via sink")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// admin logs API 可查
	var out []sysLogJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/logs?category=play&limit=10", nil), &out)
	found := false
	for _, l := range out {
		if l.Category == "play" && strings.Contains(l.Message, "测试日志消息-xyz-123") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs api: %+v", out)
	}

	// limit 默认/上限
	var out2 []sysLogJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/logs?limit=99999", nil), &out2)
	if len(out2) > 1000 {
		t.Fatalf("limit not clamped: %d", len(out2))
	}

	// DELETE 清空
	resp := adminReq(t, fx, "DELETE", "/nb/admin/api/logs", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("clear: %d", resp.StatusCode)
	}
	resp.Body.Close()
	var out3 []sysLogJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/logs", nil), &out3)
	if len(out3) != 0 {
		t.Fatalf("not cleared: %+v", out3)
	}
}

// ---------------------------------------------------------------- 直链缓存

func TestStreamCacheAdmin(t *testing.T) {
	fx := newCfgFixture(t)

	fx.srv.rememberStreamURL("tok1", StreamInfo{
		URL: "http://x/1", UserID: "u1", SHA1: "sha1",
		AccountID: "115小1", Filename: "a.mp4", UA: "Filmly",
	})
	fx.srv.rememberStreamURL("tok2", StreamInfo{
		URL: "http://x/2", UserID: "u2", SHA1: "sha2",
		AccountID: "115大", Filename: "b.mp4", UA: "Emby",
	})

	var list []CacheEntryJSON
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/cache", nil), &list)
	if len(list) != 2 {
		t.Fatalf("cache = %+v", list)
	}
	byUser := map[string]CacheEntryJSON{}
	for _, e := range list {
		byUser[e.User] = e
	}
	e1 := byUser["u1"]
	if e1.Filename != "a.mp4" || e1.SHA1 != "sha1" || e1.UA != "Filmly" ||
		e1.AccountUID != "" || e1.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("u1 = %+v", e1)
	}

	// DELETE 单条
	var del map[string]any
	decodeJSON(t, adminReq(t, fx, "DELETE", "/nb/admin/api/cache/u1/sha1", nil), &del)
	if del["ok"] != true {
		t.Fatalf("delete: %v", del)
	}
	decodeJSON(t, adminReq(t, fx, "DELETE", "/nb/admin/api/cache/u1/sha1", nil), &del)
	if del["ok"] != false {
		t.Fatalf("delete again: %v", del)
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/cache", nil), &list)
	if len(list) != 1 || list[0].User != "u2" {
		t.Fatalf("cache = %+v", list)
	}

	// 过期惰性淘汰
	fx.srv.rememberStreamURL("tok3", StreamInfo{URL: "http://x/3", UserID: "u3", SHA1: "sha3"})
	fx.srv.mu.Lock()
	ce := fx.srv.streamCache["tok3"]
	ce.expires = time.Now().Add(-time.Minute)
	fx.srv.streamCache["tok3"] = ce
	fx.srv.mu.Unlock()
	if got := fx.srv.ListCache(); len(got) != 1 {
		t.Fatalf("expired not evicted: %+v", got)
	}

	// DELETE 清空
	resp := adminReq(t, fx, "DELETE", "/nb/admin/api/cache", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("clear: %d", resp.StatusCode)
	}
	resp.Body.Close()
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/cache", nil), &list)
	if len(list) != 0 {
		t.Fatalf("not cleared: %+v", list)
	}
}

// ---------------------------------------------------------------- 控制台

func TestAdminConsole(t *testing.T) {
	fx := newCfgFixture(t)
	now := time.Now()
	mk := func(branch, ua string, ok bool, ago time.Duration) {
		t.Helper()
		if err := fx.st.LogDecision(engine.DecisionSummary{
			At: now.Add(-ago), UserID: "lzy", Branch: branch,
			Allowed: ok, AccountID: "115小1", UA: ua,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(engine.BranchCacheHit, "Filmly/1.0", true, time.Hour)
	mk(engine.BranchCacheHit, "VidHub/2.0", true, 2*time.Hour)
	mk(engine.BranchPoolSeedFallback, "网易爆米花 App", true, 3*time.Hour)
	mk(engine.BranchPoolProbeHit, "Emby Theater", true, 4*time.Hour)
	mk(engine.BranchOwnDriveHit, "curl/8.0", false, 5*time.Hour)
	mk(engine.BranchP2PRapid, "", true, 26*time.Hour) // 超出 24h 范围

	var c struct {
		Range      string         `json:"range"`
		Total      int64          `json:"total"`
		Transfer   int64          `json:"transfer"`
		Direct     int64          `json:"direct"`
		Cookie     int64          `json:"cookie"`
		SourceRate float64        `json:"sourceRate"`
		CPU        float64        `json:"cpu"`
		MemMB      float64        `json:"memMB"`
		DiskUsed   uint64         `json:"diskUsed"`
		DiskTotal  uint64         `json:"diskTotal"`
		Timeline   []timelineJSON `json:"timeline"`
		UAStats    []uaStatJSON   `json:"uaStats"`
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/console?range=24h", nil), &c)
	if c.Range != "24h" || c.Total != 5 {
		t.Fatalf("console = %+v", c)
	}
	if c.Transfer != 1 { // 只有 pool_seed_fallback 落在 24h 内
		t.Fatalf("transfer = %d", c.Transfer)
	}
	if c.Direct != 4 { // cache_hit*2 + pool_probe_hit + own_drive_hit
		t.Fatalf("direct = %d", c.Direct)
	}
	if c.Cookie != 1 { // own_drive_hit
		t.Fatalf("cookie = %d", c.Cookie)
	}
	if c.SourceRate != 0.8 { // 4/5
		t.Fatalf("sourceRate = %v", c.SourceRate)
	}
	ua := map[string]uaStatJSON{}
	for _, u := range c.UAStats {
		ua[u.Name] = u
	}
	want := map[string][2]int64{
		"Filmly": {1, 1}, "VidHub": {1, 1}, "网易爆米花": {1, 0},
		"Emby": {1, 1}, "其他": {1, 1}, // curl → 其他；own_drive_hit 属 direct
	}
	for name, w := range want {
		got, ok := ua[name]
		if !ok || got.Plays != w[0] || got.Directs != w[1] {
			t.Fatalf("ua %q = %+v, want plays=%d directs=%d (all=%v)", name, got, w[0], w[1], ua)
		}
	}
	if len(c.Timeline) == 0 {
		t.Fatal("timeline empty")
	}
	if c.MemMB <= 0 || c.DiskTotal == 0 || c.CPU < 0 || c.CPU > 100 {
		t.Fatalf("sys = cpu %v mem %v disk %v/%v", c.CPU, c.MemMB, c.DiskUsed, c.DiskTotal)
	}

	// range 非法 → 回退 24h
	var c2 struct {
		Range string `json:"range"`
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/console?range=bogus", nil), &c2)
	if c2.Range != "24h" {
		t.Fatalf("range = %q", c2.Range)
	}
}

// ---------------------------------------------------------------- 密钥回归

func TestAdminNoSecretsLeak(t *testing.T) {
	fx := newCfgFixture(t)
	ctx := context.Background()

	// 密码 hash 永不返回
	h, _ := hashPassword("pw123-secret")
	if err := fx.st.SetSetting(ctx, "admin.password_hash", h); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/settings", nil), &m)
	for k, v := range m {
		if k == "admin.password_hash" {
			t.Fatal("password_hash returned")
		}
		if strings.Contains(v, "pw123-secret") {
			t.Fatalf("password leaked in %s", k)
		}
	}

	// cookie 明文永不返回、永不打日志（日志断言：列表接口原始 body）
	as, err := fx.st.Accounts115(testCookieKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := as.UpsertAccount115(ctx, store.Account115Input{
		ID: "sec1", Kind: "pool", Cookie: "UID=topsecret-cookie;CID=x",
	}); err != nil {
		t.Fatal(err)
	}
	raw := adminBody(t, adminReq(t, fx, "GET", "/nb/admin/api/accounts", nil))
	if strings.Contains(raw, "topsecret-cookie") {
		t.Fatal("cookie plaintext leaked in accounts list")
	}
	// cookie 校验失败路径也不打日志（validateCookie 注入假失败已在 accounts 测试覆盖）
}
