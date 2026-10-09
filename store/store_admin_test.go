package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nextemby-replay/engine"
)

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, ok, err := s.GetSetting(ctx, "nope"); err != nil || ok {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if err := s.SetSetting(ctx, "console.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetSetting(ctx, "console.theme")
	if err != nil || !ok || v != "dark" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	// 覆盖
	if err := s.SetSetting(ctx, "console.theme", "light"); err != nil {
		t.Fatal(err)
	}
	v, _, _ = s.GetSetting(ctx, "console.theme")
	if v != "light" {
		t.Fatalf("overwrite: %q", v)
	}
	// 前缀查询 + LIKE 转义
	for _, kv := range [][2]string{
		{"console.title", "nb"},
		{"console.banner", "hi"},
		{"console_extra", "x"}, // 无点分隔，不应被 "console." 命中
		{"a_x", "1"},
		{"a%y", "2"},
		{"ab", "3"},
	} {
		if err := s.SetSetting(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.SettingsByPrefix(ctx, "console.")
	if err != nil || len(m) != 3 || m["console.theme"] != "light" || m["console.title"] != "nb" || m["console.banner"] != "hi" {
		t.Fatalf("prefix: %v %v", m, err)
	}
	// 下划线按字面前缀匹配（LIKE 转义）
	m, err = s.SettingsByPrefix(ctx, "a_")
	if err != nil || len(m) != 1 || m["a_x"] != "1" {
		t.Fatalf("prefix escape _: %v %v", m, err)
	}
	m, err = s.SettingsByPrefix(ctx, "a%")
	if err != nil || len(m) != 1 || m["a%y"] != "2" {
		t.Fatalf("prefix escape %%: %v %v", m, err)
	}
	// 空前缀返回全部
	all, err := s.SettingsByPrefix(ctx, "")
	if err != nil || len(all) < 7 {
		t.Fatalf("prefix all: %d %v", len(all), err)
	}
}

func TestAccounts115(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	key := []byte("0123456789abcdef0123456789abcdef") // 32B
	ac, err := s.Accounts115(key)
	if err != nil {
		t.Fatal(err)
	}

	// 种子行：115大 seed / 115小1 / 115小2 pool（kind,name 排序）
	list, err := ac.ListAccounts115(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list=%v %v", list, err)
	}
	if list[0].ID != "115小1" || list[0].Kind != "pool" || list[2].ID != "115大" || list[2].Kind != "seed" {
		t.Fatalf("order: %+v", list)
	}
	if list[0].HasCookie || list[0].MaxUsers != 4 || !list[0].Healthy || !list[0].Enabled {
		t.Fatalf("seed fields: %+v", list[0])
	}

	// 写入带 cookie 的账号
	secret := "UID=abc123&pin=xxx"
	if err := ac.UpsertAccount115(ctx, Account115Input{
		ID: "115小9", Name: "115小9", Kind: "pool", Cookie: secret,
		UID: "uid-123", MaxUsers: 4, Healthy: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ac.GetAccount115(ctx, "115小9")
	if err != nil || !got.HasCookie || got.UID != "uid-123" || got.RapidDir != "/最近接收" {
		t.Fatalf("get: %+v %v", got, err)
	}
	cookie, ok, err := ac.AccountCookie(ctx, "115小9")
	if err != nil || !ok || cookie != secret {
		t.Fatalf("cookie: %q %v %v", cookie, ok, err)
	}
	if m := CookieMask(got); m != "UID:uid-123 | ••••••••" {
		t.Fatalf("mask: %q", m)
	}
	// 换 key 不可读
	ac2, _ := s.Accounts115([]byte("ffffffffffffffffffffffffffffffff"))
	if _, _, err := ac2.AccountCookie(ctx, "115小9"); err == nil {
		t.Fatal("different key should fail decrypt")
	}
	// Cookie 为空保留旧 cookie
	if err := ac.UpsertAccount115(ctx, Account115Input{
		ID: "115小9", Name: "115小9改名", Kind: "pool", UID: "uid-123",
		MaxUsers: 4, Healthy: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	cookie, ok, err = ac.AccountCookie(ctx, "115小9")
	if err != nil || !ok || cookie != secret {
		t.Fatalf("keep cookie: %q %v %v", cookie, ok, err)
	}
	got, _ = ac.GetAccount115(ctx, "115小9")
	if got.Name != "115小9改名" {
		t.Fatalf("name not updated: %+v", got)
	}
	// 覆盖 cookie
	if err := ac.UpsertAccount115(ctx, Account115Input{
		ID: "115小9", Name: "115小9", Kind: "pool", Cookie: "NEW",
		MaxUsers: 4, Healthy: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _, _ = ac.AccountCookie(ctx, "115小9")
	if cookie != "NEW" {
		t.Fatalf("overwrite cookie: %q", cookie)
	}
	// 无 cookie 的掩码与读取
	if err := ac.UpsertAccount115(ctx, Account115Input{
		ID: "115小8", Name: "115小8", Kind: "pool", MaxUsers: 4,
		Healthy: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	got8, _ := ac.GetAccount115(ctx, "115小8")
	if CookieMask(got8) != "未绑定" {
		t.Fatalf("mask none: %q", CookieMask(got8))
	}
	if _, ok, _ := ac.AccountCookie(ctx, "115小8"); ok {
		t.Fatal("should not have cookie")
	}
	// 有 cookie 但 uid 为空
	got9b := Account115{HasCookie: true}
	if CookieMask(got9b) != "已绑定 | ••••••••" {
		t.Fatalf("mask nouid: %q", CookieMask(got9b))
	}
	// 未知账号
	if _, err := ac.GetAccount115(ctx, "115小7"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown error: %v", err)
	}
	if _, ok, _ := ac.AccountCookie(ctx, "115小7"); ok {
		t.Fatal("unknown account should not have cookie")
	}
	// 删除
	if err := ac.DeleteAccount115(ctx, "115小8"); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.GetAccount115(ctx, "115小8"); err == nil {
		t.Fatal("should be deleted")
	}
}

func TestPathMaps(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// 种子映射
	list, err := s.ListPathMaps(ctx)
	if err != nil || len(list) != 1 || list[0].EmbyPath != "/CloudNAS/CloudDrive/115open" ||
		list[0].AccountID != "115大" || list[0].SubPath != "" {
		t.Fatalf("seed: %v %v", list, err)
	}
	// 新增
	if err := s.SetPathMap(ctx, "/media/115", "115小1", "/电视剧"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListPathMaps(ctx)
	if len(list) != 2 {
		t.Fatalf("list: %v", list)
	}
	// 更新
	if err := s.SetPathMap(ctx, "/media/115", "115小2", "/电影"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListPathMaps(ctx)
	var found *PathMap
	for i := range list {
		if list[i].EmbyPath == "/media/115" {
			found = &list[i]
		}
	}
	if found == nil || found.AccountID != "115小2" || found.SubPath != "/电影" {
		t.Fatalf("update: %v", list)
	}
	// 删除
	if err := s.DeletePathMap(ctx, "/media/115"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListPathMaps(ctx)
	if len(list) != 1 {
		t.Fatalf("delete: %v", list)
	}
}

func TestTemplates(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// 种子 vip
	vip, found, err := s.GetTemplate(ctx, "vip")
	if err != nil || !found {
		t.Fatalf("seed vip: %v %v", found, err)
	}
	want := Template{Name: "vip", MaxConcurrent: 5, MaxDevices: 10, DefaultLine: "self",
		DailyPlays: 3, UIDTaskLimit: 3, LockHours: 24, DailyRapid: -1, GiftDays: 1, ExpireDeleteDays: 7}
	if vip != want {
		t.Fatalf("vip=%+v want=%+v", vip, want)
	}
	// 未知模板
	if _, found, _ := s.GetTemplate(ctx, "nope"); found {
		t.Fatal("should not find")
	}
	// 增删改
	basic := Template{Name: "basic", MaxConcurrent: 1, MaxDevices: 2, DefaultLine: "pool",
		DailyPlays: 1, UIDTaskLimit: 1, LockHours: 12, DailyRapid: 5, GiftDays: 0, ExpireDeleteDays: 3}
	if err := s.UpsertTemplate(ctx, basic); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTemplates(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "basic" || list[1].Name != "vip" {
		t.Fatalf("list: %v %v", list, err)
	}
	basic.DailyPlays = 9
	if err := s.UpsertTemplate(ctx, basic); err != nil {
		t.Fatal(err)
	}
	got, found, _ := s.GetTemplate(ctx, "basic")
	if !found || got.DailyPlays != 9 {
		t.Fatalf("update: %+v", got)
	}
	if err := s.DeleteTemplate(ctx, "basic"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetTemplate(ctx, "basic"); found {
		t.Fatal("should be deleted")
	}
}

func TestSysLogs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.WriteLog(ctx, "system", "boot"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLog(ctx, "admin", "login"); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListLogs(ctx, "", 10)
	if err != nil || len(all) != 2 || all[0].Message != "login" || all[0].Category != "admin" {
		t.Fatalf("list: %v %v", all, err)
	}
	// 分类过滤
	admin, err := s.ListLogs(ctx, "admin", 10)
	if err != nil || len(admin) != 1 {
		t.Fatalf("filter: %v %v", admin, err)
	}
	// limit 上限
	many, err := s.ListLogs(ctx, "", 5000)
	if err != nil || len(many) != 2 {
		t.Fatalf("limit cap: %d %v", len(many), err)
	}
	// trim：写 100 条，用小阈值验证内部函数
	for i := 0; i < 100; i++ {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO syslogs(ts, category, message) VALUES(?,?,?)`,
			nowUnix(), "bulk", "m"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.trimSysLogs(ctx, 50); err != nil {
		t.Fatal(err)
	}
	var n, minID, maxID int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(id), MAX(id) FROM syslogs`).Scan(&n, &minID, &maxID); err != nil {
		t.Fatal(err)
	}
	if n != 50 || minID != 53 || maxID != 102 {
		t.Fatalf("trim: n=%d min=%d max=%d", n, minID, maxID)
	}
	// 低于阈值时不删
	if err := s.trimSysLogs(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM syslogs`).Scan(&n); err != nil || n != 50 {
		t.Fatalf("trim noop: %d %v", n, err)
	}
	// 清空
	if err := s.ClearLogs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM syslogs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("clear: %d %v", n, err)
	}
}

func TestUsersNewColumns(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u := s.Users()

	// 种子用户的默认值
	lzy, err := u.GetUser(ctx, "lzy")
	if err != nil || lzy.ExpiresAt != 0 || lzy.Banned || lzy.Remark != "" {
		t.Fatalf("seed defaults: %+v %v", lzy, err)
	}
	// 三列读写
	in := engine.User{ID: "adm", Mode: "pool", Template: "vip",
		ExpiresAt: 1893456000, Banned: true, Remark: "测试备注"}
	if err := u.UpdateUser(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := u.GetUser(ctx, "adm")
	if err != nil || got != in {
		t.Fatalf("roundtrip: %+v want %+v err=%v", got, in, err)
	}
	// 列表包含新列
	list, err := u.ListUsers(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v %v", list, err)
	}
	var adm *engine.User
	for i := range list {
		if list[i].ID == "adm" {
			adm = &list[i]
		}
	}
	if adm == nil || *adm != in {
		t.Fatalf("list roundtrip: %+v", adm)
	}
	// 更新后三列同步
	in.Banned = false
	in.Remark = "改"
	if err := u.UpdateUser(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, _ = u.GetUser(ctx, "adm")
	if got.Banned || got.Remark != "改" {
		t.Fatalf("update: %+v", got)
	}
}

func TestDecisionsUA(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	mk := func(ua string) engine.DecisionSummary {
		return engine.DecisionSummary{
			At: time.Now(), UserID: "lzy", Branch: engine.BranchCacheHit,
			Allowed: true, AccountID: "115小1", UA: ua,
			Steps: []engine.StepTrace{{Step: "cache", Branch: "hit"}},
		}
	}
	if err := s.LogDecision(mk("VidHub/1.0")); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(mk("")); err != nil {
		t.Fatal(err)
	}
	recs, err := s.RecentDecisions(ctx, 10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%d %v", len(recs), err)
	}
	if recs[0].UA != "" || recs[1].UA != "VidHub/1.0" {
		t.Fatalf("ua readback: %+v", recs)
	}
	urecs, err := s.RecentUserDecisions(ctx, "lzy", 10)
	if err != nil || len(urecs) != 2 || urecs[1].UA != "VidHub/1.0" {
		t.Fatalf("user ua: %+v %v", urecs, err)
	}
}

func TestConsoleStats(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	t0 := int64(1700000000)

	mk := func(user, branch string, ok bool, ua string, at int64) engine.DecisionSummary {
		return engine.DecisionSummary{
			At: time.Unix(at, 0), UserID: user, Branch: branch, Allowed: ok,
			AccountID: "115小1", UA: ua,
			Steps: []engine.StepTrace{{Step: "s", Branch: "b"}},
		}
	}
	rows := []engine.DecisionSummary{
		mk("lzy", engine.BranchCacheHit, true, "UA-A", t0),             // 直连
		mk("lzy", engine.BranchPoolSeedFallback, true, "UA-A", t0+100), // 秒传
		mk("xrq", engine.BranchDeniedConcurrency, false, "", t0+3700),  // 失败，空 ua
		mk("lzy", engine.BranchP2PRapid, true, "UA-B", t0+3700),        // 秒传
	}
	for _, r := range rows {
		if err := s.LogDecision(r); err != nil {
			t.Fatal(err)
		}
	}
	since := t0 - 10

	n, err := s.CountUserDecisionsSince(ctx, "lzy", since)
	if err != nil || n != 3 {
		t.Fatalf("count decisions: %d %v", n, err)
	}
	n, err = s.CountUserDecisionsSince(ctx, "nobody", since)
	if err != nil || n != 0 {
		t.Fatalf("count decisions empty: %d %v", n, err)
	}
	n, err = s.CountUserRapidSince(ctx, "lzy", since)
	if err != nil || n != 2 {
		t.Fatalf("count rapid: %d %v", n, err)
	}
	n, err = s.CountUserRapidSince(ctx, "xrq", since)
	if err != nil || n != 0 {
		t.Fatalf("count rapid xrq: %d %v", n, err)
	}

	agg, err := s.DecisionRangeAgg(ctx, since, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Total != 4 || agg.OK != 3 {
		t.Fatalf("total/ok: %+v", agg)
	}
	wantBranch := map[string]engine.StatPair{
		engine.BranchCacheHit:          {OK: 1},
		engine.BranchPoolSeedFallback:  {OK: 1},
		engine.BranchDeniedConcurrency: {Fail: 1},
		engine.BranchP2PRapid:          {OK: 1},
	}
	if len(agg.ByBranch) != 4 {
		t.Fatalf("bybranch len: %v", agg.ByBranch)
	}
	for b, sp := range wantBranch {
		if agg.ByBranch[b] != sp {
			t.Fatalf("bybranch %s: %+v want %+v", b, agg.ByBranch[b], sp)
		}
	}
	b1 := (t0 / 3600) * 3600
	b2 := ((t0 + 3700) / 3600) * 3600
	if len(agg.Timeline) != 2 || agg.Timeline[0] != (TimelinePoint{T: b1, Plays: 2}) ||
		agg.Timeline[1] != (TimelinePoint{T: b2, Plays: 2}) {
		t.Fatalf("timeline: %+v", agg.Timeline)
	}
	wantUA := []UAPlay{
		{UA: "UA-A", Plays: 2, Directs: 1}, // cache_hit 直连，pool_seed_fallback 不算
		{UA: "UA-B", Plays: 1, Directs: 0},
		{UA: "(unknown)", Plays: 1, Directs: 0},
	}
	if len(agg.ByUA) != 3 {
		t.Fatalf("byua len: %+v", agg.ByUA)
	}
	for i, w := range wantUA {
		if agg.ByUA[i] != w {
			t.Fatalf("byua[%d]: %+v want %+v", i, agg.ByUA[i], w)
		}
	}
}

func TestUserActivity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()

	mk := func(ua string, at time.Time) engine.DecisionSummary {
		return engine.DecisionSummary{
			At: at, UserID: "act", Branch: engine.BranchCacheHit, Allowed: true,
			AccountID: "115小1", UA: ua,
			Steps: []engine.StepTrace{{Step: "s", Branch: "b"}},
		}
	}
	// 一条 40 天前的（不计入 30 天）
	if err := s.LogDecision(mk("OLD", now.Add(-40*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(mk("UA-A1", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(mk("", now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(mk("UA-A2", now)); err != nil {
		t.Fatal(err)
	}
	plays, lastUA, err := s.UserActivity(ctx, "act")
	if err != nil || plays != 3 || lastUA != "UA-A2" {
		t.Fatalf("activity: plays=%d ua=%q err=%v", plays, lastUA, err)
	}
	// 专门造一个只有空 ua 的用户
	onlyEmpty := engine.DecisionSummary{
		At: now, UserID: "empty", Branch: engine.BranchCacheHit, Allowed: true,
		AccountID: "115小1", UA: "",
		Steps: []engine.StepTrace{{Step: "s", Branch: "b"}},
	}
	if err := s.LogDecision(onlyEmpty); err != nil {
		t.Fatal(err)
	}
	plays, lastUA, err = s.UserActivity(ctx, "empty")
	if err != nil || plays != 1 || lastUA != "" {
		t.Fatalf("empty ua: plays=%d ua=%q err=%v", plays, lastUA, err)
	}
	// 从无记录的用户
	plays, lastUA, err = s.UserActivity(ctx, "ghost")
	if err != nil || plays != 0 || lastUA != "" {
		t.Fatalf("ghost: plays=%d ua=%q err=%v", plays, lastUA, err)
	}
}

// TestMigrateOldDB：先手工建一个只有 pool_accounts / 老结构 users / 老结构 decisions
// 的库，再 Open，验证迁移幂等生效。
func TestMigrateOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE pool_accounts(id TEXT PRIMARY KEY, max_users INTEGER NOT NULL DEFAULT 4, healthy INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL)`,
		`INSERT INTO pool_accounts VALUES('115小5', 3, 0, 1700000000)`,
		`INSERT INTO pool_accounts VALUES('115小6', 4, 1, 1700000000)`,
		`CREATE TABLE users(id TEXT PRIMARY KEY, mode TEXT NOT NULL DEFAULT 'pool', template TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO users VALUES('old', '115', 'vip', 1, 2)`,
		`CREATE TABLE decisions(id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', account_id TEXT NOT NULL DEFAULT '', sha1 TEXT NOT NULL DEFAULT '', elapsed_ms INTEGER NOT NULL DEFAULT 0, ok INTEGER NOT NULL DEFAULT 1, steps_json TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL)`,
		`INSERT INTO decisions(user_id, branch, account_id, created_at) VALUES('old', 'cache_hit', '115小5', 1700000000)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	// pool_accounts → accounts115（kind=pool，healthy/max_users 保留）
	ac, err := s.Accounts115([]byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	a5, err := ac.GetAccount115(ctx, "115小5")
	if err != nil || a5.Kind != "pool" || a5.MaxUsers != 3 || a5.Healthy {
		t.Fatalf("migrate 115小5: %+v %v", a5, err)
	}
	a6, err := ac.GetAccount115(ctx, "115小6")
	if err != nil || !a6.Healthy || a6.MaxUsers != 4 {
		t.Fatalf("migrate 115小6: %+v %v", a6, err)
	}
	// PoolStore 也能读到（enabled=1）
	accs, err := s.Pool().ListAccounts(ctx)
	if err != nil || len(accs) != 2 {
		t.Fatalf("pool list: %v %v", accs, err)
	}
	// users 老行读出新列默认值
	u, err := s.Users().GetUser(ctx, "old")
	if err != nil || u.Mode != "115" || u.Template != "vip" || u.ExpiresAt != 0 || u.Banned || u.Remark != "" {
		t.Fatalf("migrate user: %+v %v", u, err)
	}
	// decisions 老行读出 ua=''
	recs, err := s.RecentDecisions(ctx, 10)
	if err != nil || len(recs) != 1 || recs[0].UA != "" || recs[0].Branch != "cache_hit" {
		t.Fatalf("migrate decision: %+v %v", recs, err)
	}
	// 再 Open 一次验证幂等
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	list, err := s2.Pool().ListAccounts(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("idempotent: %v %v", list, err)
	}
}

// TestEnsureDefaultsSeeds 验证 EnsureDefaults 的新种子（且调两次幂等）。
func TestEnsureDefaultsSeeds(t *testing.T) {
	ctx := context.Background()
	s := openTest(t) // 已调一次

	ac, _ := s.Accounts115([]byte("key"))
	list, err := ac.ListAccounts115(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("accounts115 seeds: %v %v", list, err)
	}
	var seed *Account115
	for i := range list {
		if list[i].ID == "115大" {
			seed = &list[i]
		}
	}
	if seed == nil || seed.Kind != "seed" || seed.HasCookie || seed.RapidDir != "/最近接收" {
		t.Fatalf("115大: %+v", seed)
	}
	pm, err := s.ListPathMaps(ctx)
	if err != nil || len(pm) != 1 || pm[0].EmbyPath != "/CloudNAS/CloudDrive/115open" || pm[0].AccountID != "115大" {
		t.Fatalf("pathmap seed: %v %v", pm, err)
	}
	tpl, found, err := s.GetTemplate(ctx, "vip")
	if err != nil || !found || tpl.MaxConcurrent != 5 || tpl.DefaultLine != "self" || tpl.DailyPlays != 3 {
		t.Fatalf("vip seed: %+v %v %v", tpl, found, err)
	}

	// 再调一次：幂等，不报错不翻倍
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = ac.ListAccounts115(ctx)
	pm, _ = s.ListPathMaps(ctx)
	tpls, _ := s.ListTemplates(ctx)
	if len(list) != 3 || len(pm) != 1 || len(tpls) != 1 {
		t.Fatalf("idempotent: accounts=%d pathmaps=%d templates=%d", len(list), len(pm), len(tpls))
	}
}
