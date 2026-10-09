package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nextemby-replay/engine"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("ensure defaults: %v", err)
	}
	return s
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u := s.Users()

	// 种子用户
	got, err := u.GetUser(ctx, "lzy")
	if err != nil || got.Mode != "pool" || got.Template != "vip" {
		t.Fatalf("seed lzy: %+v %v", got, err)
	}
	// 未知用户
	if _, err := u.GetUser(ctx, "nobody"); err == nil {
		t.Fatal("expected unknown user error")
	}
	// upsert 新增 + 更新
	if err := u.UpdateUser(ctx, engine.User{ID: "xrq13", Mode: "115", Template: "vip"}); err != nil {
		t.Fatal(err)
	}
	got, _ = u.GetUser(ctx, "xrq13")
	if got.Mode != "115" {
		t.Fatalf("mode=%s", got.Mode)
	}
	if err := u.UpdateUser(ctx, engine.User{ID: "xrq13", Mode: "pool", Template: ""}); err != nil {
		t.Fatal(err)
	}
	got, _ = u.GetUser(ctx, "xrq13")
	if got.Mode != "pool" || got.Template != "" {
		t.Fatalf("after update: %+v", got)
	}
	// 列表按 ID 排序
	list, err := u.ListUsers(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list=%v %v", list, err)
	}
	if list[0].ID != "guest" || list[1].ID != "lzy" || list[2].ID != "xrq13" {
		t.Fatalf("order: %v", list)
	}
}

func TestPlaybackRecords(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := s.Records()

	if _, found, _ := r.FindRecentPlayer(ctx, "SHA1A", "lzy"); found {
		t.Fatal("should not find")
	}
	// lzy 先播，xrq13 后播
	if err := r.RecordPlayback(ctx, "lzy", "SHA1A"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // played_at 秒精度，错开
	if err := r.RecordPlayback(ctx, "xrq13", "SHA1A"); err != nil {
		t.Fatal(err)
	}
	// 排除自己后应返回最近的 xrq13
	who, found, err := r.FindRecentPlayer(ctx, "SHA1A", "lzy")
	if err != nil || !found || who != "xrq13" {
		t.Fatalf("who=%q found=%v err=%v", who, found, err)
	}
	// 排除 xrq13 后返回 lzy
	who, found, _ = r.FindRecentPlayer(ctx, "SHA1A", "xrq13")
	if !found || who != "lzy" {
		t.Fatalf("who=%q found=%v", who, found)
	}
	// 幂等：重复记录不报错
	if err := r.RecordPlayback(ctx, "lzy", "SHA1A"); err != nil {
		t.Fatal(err)
	}
}

func TestPool(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := s.Pool()

	accs, err := p.ListAccounts(ctx)
	if err != nil || len(accs) != 2 {
		t.Fatalf("accs=%v %v", accs, err)
	}
	if accs[0].ID != "115小1" || !accs[0].Healthy || accs[0].MaxUsersOrDefault() != 4 {
		t.Fatalf("accs[0]=%+v", accs[0])
	}
	// EnsureAccount 幂等，不覆盖已有
	if err := p.EnsureAccount(ctx, "115小1", 8); err != nil {
		t.Fatal(err)
	}
	accs, _ = p.ListAccounts(ctx)
	if accs[0].MaxUsersOrDefault() != 4 {
		t.Fatalf("ensure overwrote: %+v", accs[0])
	}
	// 新增账号
	if err := p.EnsureAccount(ctx, "115小3", 3); err != nil {
		t.Fatal(err)
	}
	// 摘除/恢复
	if err := p.SetHealthy(ctx, "115小1", false); err != nil {
		t.Fatal(err)
	}
	accs, _ = p.ListAccounts(ctx)
	if accs[0].Healthy {
		t.Fatal("should be unhealthy")
	}
	if err := p.SetHealthy(ctx, "115小1", true); err != nil {
		t.Fatal(err)
	}
	// 未知账号：错误含 "unknown"（管理后台据此 404）
	if err := p.SetHealthy(ctx, "115小9", false); err == nil {
		t.Fatal("expected unknown account error")
	}
}

func TestDecisions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()

	mk := func(user, branch string, ok bool, at time.Time) engine.DecisionSummary {
		return engine.DecisionSummary{
			At: at, UserID: user, Branch: branch, Allowed: ok,
			AccountID: "115小1",
			Steps:     []engine.StepTrace{{Step: "cache", Branch: "hit", Duration: 5 * time.Millisecond}},
		}
	}
	if err := s.LogDecision(mk("lzy", engine.BranchCacheHit, true, now)); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(mk("lzy", engine.BranchPoolSeedFallback, true, now)); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(mk("xrq13", engine.BranchDeniedConcurrency, false, now)); err != nil {
		t.Fatal(err)
	}

	// 最近倒序
	recs, err := s.RecentDecisions(ctx, 10)
	if err != nil || len(recs) != 3 {
		t.Fatalf("recs=%d %v", len(recs), err)
	}
	if recs[0].Branch != engine.BranchDeniedConcurrency || recs[2].Branch != engine.BranchCacheHit {
		t.Fatalf("order: %v", recs)
	}
	if recs[0].ElapsedMs() != 5 {
		t.Fatalf("elapsed=%d", recs[0].ElapsedMs())
	}
	// limit
	recs, _ = s.RecentDecisions(ctx, 2)
	if len(recs) != 2 {
		t.Fatalf("limit: %d", len(recs))
	}
	// 分支统计
	bs, err := s.BranchStats(ctx, now.Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if bs[engine.BranchCacheHit] != 1 || bs[engine.BranchDeniedConcurrency] != 1 || len(bs) != 3 {
		t.Fatalf("branch stats: %v", bs)
	}
	// 用户统计聚合
	us, err := s.UserStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if us["lzy"] != (engine.StatPair{OK: 2, Fail: 0}) {
		t.Fatalf("lzy stats: %+v", us["lzy"])
	}
	if us["xrq13"] != (engine.StatPair{OK: 0, Fail: 1}) {
		t.Fatalf("xrq13 stats: %+v", us["xrq13"])
	}
	// 按用户查
	urecs, err := s.RecentUserDecisions(ctx, "lzy", 10)
	if err != nil || len(urecs) != 2 {
		t.Fatalf("user recs: %d %v", len(urecs), err)
	}
}

func TestCookies(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	key := []byte("0123456789abcdef0123456789abcdef") // 32B
	cs, err := s.Cookies(key)
	if err != nil {
		t.Fatal(err)
	}
	// roundtrip
	secret := "UID=aaa;CID=bbb;SEID=ccc"
	if err := cs.SetCookie(ctx, "lzy", secret); err != nil {
		t.Fatal(err)
	}
	if !cs.HasCookie(ctx, "lzy") {
		t.Fatal("has cookie")
	}
	got, ok, err := cs.GetCookie(ctx, "lzy")
	if err != nil || !ok || got != secret {
		t.Fatalf("got=%q ok=%v err=%v", got, ok, err)
	}
	// 未设置
	if _, ok, _ := cs.GetCookie(ctx, "ghost"); ok {
		t.Fatal("ghost should not have cookie")
	}
	// 换密钥不可读
	cs2, _ := s.Cookies([]byte("ffffffffffffffffffffffffffffffff"))
	if _, _, err := cs2.GetCookie(ctx, "lzy"); err == nil {
		t.Fatal("different key should fail decrypt")
	}
	// 短密钥规整可用
	cs3, err := s.Cookies([]byte("short"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cs3.SetCookie(ctx, "lzy", secret); err != nil {
		t.Fatal(err)
	}
	// 删除
	if err := cs.DeleteCookie(ctx, "lzy"); err != nil {
		t.Fatal(err)
	}
	if cs.HasCookie(ctx, "lzy") {
		t.Fatal("should be deleted")
	}
}

func TestEnsureDefaultsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// 改一个种子用户后再跑，应保持修改（DO NOTHING）
	if err := s.Users().UpdateUser(ctx, engine.User{ID: "lzy", Mode: "115", Template: "vip"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Users().GetUser(ctx, "lzy")
	if got.Mode != "115" {
		t.Fatalf("ensure overwrote user: %+v", got)
	}
}
