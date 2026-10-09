package engine_test

import (
	"context"
	"testing"

	"nextemby-replay/engine"
	"nextemby-replay/engine/memory"
)

func poolFixture(t *testing.T, userID string) *fixture {
	t.Helper()
	return newFixture(
		engine.Config{Templates: map[string]int{"vip": 100}},
		[]engine.User{{ID: userID, Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}},
	)
}

func doHandle(t *testing.T, f *fixture, userID, sha1 string) engine.Decision {
	t.Helper()
	d, err := f.eng.Handle(context.Background(), req(userID, sha1))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if d.Release != nil {
		d.Release()
	}
	return d
}

func TestRecentDecisionsRing(t *testing.T) {
	f := poolFixture(t, "u1")
	d1 := doHandle(t, f, "u1", "SHA-A")
	if d1.Branch != engine.BranchPoolSeedFallback {
		t.Fatalf("branch = %q", d1.Branch)
	}
	doHandle(t, f, "u1", "SHA-B")

	got := f.eng.RecentDecisions(10)
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].UserID != "u1" || got[0].Branch != engine.BranchPoolSeedFallback {
		t.Fatalf("newest = %+v", got[0])
	}
	if got[0].At.IsZero() || got[0].ElapsedMs() < 0 {
		t.Fatalf("summary fields missing: %+v", got[0])
	}
	// limit 生效
	if got := f.eng.RecentDecisions(1); len(got) != 1 {
		t.Fatalf("limit len = %d", len(got))
	}
}

func TestRecentDecisionsCap(t *testing.T) {
	f := poolFixture(t, "u1")
	for i := 0; i < 205; i++ {
		doHandle(t, f, "u1", "SHA")
	}
	if got := f.eng.RecentDecisions(0); len(got) != 200 {
		t.Fatalf("ring len = %d, want 200", len(got))
	}
}

func TestPoolLocksView(t *testing.T) {
	f := poolFixture(t, "u1")
	doHandle(t, f, "u1", "SHA-A") // rapid transfer → 建 24h 锁

	locks := f.eng.PoolLocks()
	if len(locks) != 1 || locks[0].UserID != "u1" || locks[0].AccountID != "115小1" {
		t.Fatalf("locks = %+v", locks)
	}
	if locks[0].ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt 未设置")
	}
	// 25h 后过期
	f.clock.advance(25 * 3600 * 1000000000)
	if locks := f.eng.PoolLocks(); len(locks) != 0 {
		t.Fatalf("过期后 locks = %+v", locks)
	}
}

func TestAllUserStatsView(t *testing.T) {
	f := poolFixture(t, "u1")
	doHandle(t, f, "u1", "SHA-A")
	doHandle(t, f, "u1", "SHA-B")

	m := f.eng.AllUserStats()
	st, ok := m["u1"]
	if !ok || st.OK != 2 || st.Fail != 0 {
		t.Fatalf("stats = %+v", m)
	}
}

func TestSetOwnDrive(t *testing.T) {
	f := newFixture(
		engine.Config{},
		[]engine.User{{ID: "u2", Mode: engine.ModeOwn115}},
		nil,
	)
	// 未绑定自有盘 → 拒绝
	d, err := f.eng.Handle(context.Background(), req("u2", "SHA-A"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.Branch != engine.BranchDeniedNoOwnDrive {
		t.Fatalf("d = %+v", d)
	}
	if d.Release != nil {
		d.Release()
	}

	// 个人中心绑定后 → 115 模式链路走通（STEP3 兜底）
	drv := memory.NewDriveClient("u2-115")
	f.eng.SetOwnDrive("u2", drv)
	d = doHandle(t, f, "u2", "SHA-A")
	if !d.Allowed || d.Branch != engine.BranchSeedFallback {
		t.Fatalf("d = %+v", d)
	}
	if !drv.HasFile("SHA-A") {
		t.Fatal("秒传后文件应落盘")
	}
}

func TestMemoryUserStoreUpdateAndList(t *testing.T) {
	ctx := context.Background()
	s := memory.NewUserStore(
		engine.User{ID: "b", Mode: engine.ModePool, Template: "vip"},
		engine.User{ID: "a", Mode: engine.ModePool, Template: "vip"},
	)
	// 更新
	if err := s.UpdateUser(ctx, engine.User{ID: "a", Mode: engine.ModeOwn115, Template: "vip"}); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUser(ctx, "a")
	if err != nil || u.Mode != engine.ModeOwn115 {
		t.Fatalf("u = %+v err = %v", u, err)
	}
	// 新增（upsert）
	if err := s.UpdateUser(ctx, engine.User{ID: "c", Mode: engine.ModePool}); err != nil {
		t.Fatal(err)
	}
	// 列表按 ID 排序
	us, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 3 || us[0].ID != "a" || us[1].ID != "b" || us[2].ID != "c" {
		t.Fatalf("users = %+v", us)
	}
	// 类型断言：满足可选接口
	var _ engine.UserUpdater = s
	var _ engine.UserLister = s
}
