package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"nextemby-replay/engine"
	"nextemby-replay/engine/memory"
)

// fakeClock 是可手动推进的时钟，用于确定性测试缓存/TTL。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture 聚合一次测试所需的引擎与所有假实现。
type fixture struct {
	eng     *engine.Engine
	clock   *fakeClock
	users   *memory.UserStore
	pool    *memory.PoolStore
	own     map[string]*memory.DriveClient
	poolDrv map[string]*memory.DriveClient
	shield  *memory.ShieldClient
	records *memory.PlaybackRecordStore
}

func newFixture(cfg engine.Config, users []engine.User, accounts []engine.PoolAccount) *fixture {
	clock := newFakeClock()
	cfg.Now = clock.now
	f := &fixture{
		clock:   clock,
		users:   memory.NewUserStore(users...),
		pool:    memory.NewPoolStore(accounts...),
		own:     map[string]*memory.DriveClient{},
		poolDrv: map[string]*memory.DriveClient{},
		shield:  memory.NewShieldClient(nil),
		records: memory.NewPlaybackRecordStore(),
	}
	poolDrv := map[string]engine.DriveClient{}
	for _, a := range accounts {
		d := memory.NewDriveClient(a.ID)
		f.poolDrv[a.ID] = d
		poolDrv[a.ID] = d
	}
	f.eng = engine.New(cfg, engine.Deps{
		Users:   f.users,
		Pool:    f.pool,
		Seed:    memory.NewDriveClient("115大"),
		Own:     map[string]engine.DriveClient{},
		PoolDrv: poolDrv,
		Shield:  f.shield,
		Records: f.records,
	})
	return f
}

func req(userID, sha1 string) engine.PlaybackRequest {
	return engine.PlaybackRequest{
		UserID:    userID,
		FileSHA1:  sha1,
		FileName:  "test.mkv",
		FileSize:  1024,
		Client:    "Filmly",
		TMDBID:    "935597",
		MediaType: "movie",
	}
}

const (
	shaA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	shaB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	shaC = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

var vipCfg = engine.Config{Templates: map[string]int{"vip": 2}}

// ---------------------------------------------------------------- 并发策略

func TestConcurrencyLimitDenyAndRelease(t *testing.T) {
	f := newFixture(vipCfg,
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}})
	f.poolDrv["115小1"].RapidTransfer(context.Background(), shaA, "t", 1<<20) // 预置文件

	d1, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d1.Allowed {
		t.Fatalf("first playback should be allowed: %v %+v", err, d1)
	}
	d2, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d2.Allowed {
		t.Fatalf("second playback should be allowed: %v %+v", err, d2)
	}
	// 第三路：超限拒绝（vip 上限 2）
	d3, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil {
		t.Fatalf("deny should not be an error: %v", err)
	}
	if d3.Allowed || d3.Branch != engine.BranchDeniedConcurrency || d3.DenyReason != "concurrency_limit" {
		t.Fatalf("expected concurrency deny, got %+v", d3)
	}
	if d3.Release != nil {
		t.Fatal("denied decision must not carry a Release func")
	}
	if got := f.eng.Sessions("u1"); got != 2 {
		t.Fatalf("sessions should stay 2 after deny, got %d", got)
	}
	// 释放一路，新的请求应通过
	d1.Release()
	d4, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d4.Allowed {
		t.Fatalf("playback after release should be allowed: %v %+v", err, d4)
	}
	d2.Release()
	d4.Release()
	if got := f.eng.Sessions("u1"); got != 0 {
		t.Fatalf("sessions should be 0 after all released, got %d", got)
	}
}

// ---------------------------------------------------------------- 直链缓存

func TestDirectURLCacheHitAndExpiry(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	drv := f.poolDrv["115小1"]
	if _, err := drv.RapidTransfer(context.Background(), shaA, "t", 1<<20); err != nil {
		t.Fatal(err)
	}

	d1, _ := f.eng.Handle(context.Background(), req("u1", shaA))
	d1.Release()
	if d1.Branch != engine.BranchPoolProbeHit {
		t.Fatalf("first should be pool_probe_hit, got %s", d1.Branch)
	}
	if drv.DirectURLs != 1 {
		t.Fatalf("expected 1 DirectURL call, got %d", drv.DirectURLs)
	}

	// 10min 内：缓存命中，不再调取直链
	d2, _ := f.eng.Handle(context.Background(), req("u1", shaA))
	d2.Release()
	if d2.Branch != engine.BranchCacheHit {
		t.Fatalf("second should be cache_hit, got %s", d2.Branch)
	}
	if d2.DirectURL != d1.DirectURL {
		t.Fatal("cache hit should return the same URL")
	}
	if drv.DirectURLs != 1 {
		t.Fatalf("DirectURL should not be called on cache hit, got %d", drv.DirectURLs)
	}

	// 推进 11 分钟：缓存过期，重新探测+取直链
	f.clock.advance(11 * time.Minute)
	d3, _ := f.eng.Handle(context.Background(), req("u1", shaA))
	d3.Release()
	if d3.Branch != engine.BranchPoolProbeHit {
		t.Fatalf("after expiry should be pool_probe_hit, got %s", d3.Branch)
	}
	if drv.DirectURLs != 2 {
		t.Fatalf("expected 2 DirectURL calls after expiry, got %d", drv.DirectURLs)
	}
}

// ---------------------------------------------------------------- 24h 锁定路由

func TestLock24hRouting(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{
			{ID: "115小1", Healthy: true, MaxUsers: 4},
			{ID: "115小2", Healthy: true, MaxUsers: 4},
		})
	// 两个账号都没有文件 → 走源网盘秒传分支，建锁
	d1, _ := f.eng.Handle(context.Background(), req("u1", shaA))
	d1.Release()
	if d1.Branch != engine.BranchPoolSeedFallback {
		t.Fatalf("expected pool_seed_fallback, got %s", d1.Branch)
	}
	first := d1.AccountID

	// 换一个文件再播：仍应路由到同一账号（24h 锁）
	d2, _ := f.eng.Handle(context.Background(), req("u1", shaB))
	d2.Release()
	if d2.AccountID != first {
		t.Fatalf("locked routing: expected %s, got %s", first, d2.AccountID)
	}
	if !containsBranch(d2.Steps, "resolve_account", "locked") {
		t.Fatalf("expected resolve_account:locked step, got %s", d2.Summary())
	}

	// 推进 25 小时：锁过期，应重新分配（此时两账号负载均为 0，取首个）
	f.clock.advance(25 * time.Hour)
	d3, _ := f.eng.Handle(context.Background(), req("u1", shaC))
	d3.Release()
	if !containsBranch(d3.Steps, "resolve_account", "assigned") {
		t.Fatalf("expected resolve_account:assigned after lock expiry, got %s", d3.Summary())
	}
}

func containsBranch(steps []engine.StepTrace, step, branch string) bool {
	for _, s := range steps {
		if s.Step == step && s.Branch == branch {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 池分配上限

func TestPoolCapacityAndHealth(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{
			{ID: "u1", Mode: engine.ModePool, Template: "vip"},
			{ID: "u2", Mode: engine.ModePool, Template: "vip"},
			{ID: "u3", Mode: engine.ModePool, Template: "vip"},
			{ID: "u4", Mode: engine.ModePool, Template: "vip"},
		},
		[]engine.PoolAccount{
			{ID: "115小1", Healthy: true, MaxUsers: 1},
			{ID: "115小2", Healthy: true, MaxUsers: 1},
		})

	d1, _ := f.eng.Handle(context.Background(), req("u1", shaA))
	d1.Release()
	d2, _ := f.eng.Handle(context.Background(), req("u2", shaA))
	d2.Release()
	if d1.AccountID == d2.AccountID {
		t.Fatalf("users should spread across accounts, both got %s", d1.AccountID)
	}
	// 第三个用户：池满，拒绝
	d3, _ := f.eng.Handle(context.Background(), req("u3", shaA))
	if d3.Allowed || d3.Branch != engine.BranchDeniedPoolFull {
		t.Fatalf("expected pool_exhausted deny, got %+v", d3)
	}

	// 把 115小2 标为不健康：u1 的锁仍占着 115小1，新用户 u4 无处可去 → 拒绝
	if err := f.eng.SetPoolAccountHealthy(context.Background(), "115小2", false); err != nil {
		t.Fatal(err)
	}
	d4, _ := f.eng.Handle(context.Background(), req("u4", shaB))
	if d4.Allowed {
		t.Fatalf("unhealthy+full pool should deny, got %+v", d4)
	}

	// 锁过期 + 恢复健康 → 应恢复分配
	f.clock.advance(25 * time.Hour)
	if err := f.eng.SetPoolAccountHealthy(context.Background(), "115小2", true); err != nil {
		t.Fatal(err)
	}
	d5, _ := f.eng.Handle(context.Background(), req("u4", shaB))
	d5.Release()
	if !d5.Allowed {
		t.Fatalf("should be allowed after recovery, got %+v", d5)
	}
}

// ---------------------------------------------------------------- 115 模式三步链路

func buildOwn115Fixture(t *testing.T) (*fixture, *memory.DriveClient) {
	t.Helper()
	own := memory.NewDriveClient("u1-115")
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModeOwn115, Template: "vip"}},
		nil)
	f.eng = engine.New(engine.Config{Templates: map[string]int{"vip": 10}, Now: f.clock.now}, engine.Deps{
		Users:   f.users,
		Pool:    f.pool,
		Seed:    memory.NewDriveClient("115大"),
		Own:     map[string]engine.DriveClient{"u1": own},
		PoolDrv: map[string]engine.DriveClient{},
		Shield:  f.shield,
		Records: f.records,
	})
	return f, own
}

func TestOwn115Step1Hit(t *testing.T) {
	f, own := buildOwn115Fixture(t)
	if _, err := own.RapidTransfer(context.Background(), shaA, "t", 1<<20); err != nil {
		t.Fatal(err)
	}
	own.Transfers = 0 // 清零，只统计决策链内的调用

	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d.Allowed {
		t.Fatalf("should be allowed: %v %+v", err, d)
	}
	defer d.Release()
	if d.Branch != engine.BranchOwnDriveHit {
		t.Fatalf("expected own_drive_hit, got %s (%s)", d.Branch, d.Summary())
	}
	if d.AccountID != "u1-115" {
		t.Fatalf("account should be own drive, got %s", d.AccountID)
	}
	if own.Transfers != 0 {
		t.Fatalf("STEP1 hit should not transfer, got %d transfers", own.Transfers)
	}
}

func TestOwn115Step2P2P(t *testing.T) {
	f, own := buildOwn115Fixture(t)
	// 别人播过这个文件（写入播放记录库），自己盘里没有
	if err := f.records.RecordPlayback(context.Background(), "other", shaA); err != nil {
		t.Fatal(err)
	}
	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d.Allowed {
		t.Fatalf("should be allowed: %v %+v", err, d)
	}
	defer d.Release()
	if d.Branch != engine.BranchP2PRapid {
		t.Fatalf("expected p2p_rapid, got %s (%s)", d.Branch, d.Summary())
	}
	if own.Transfers != 1 {
		t.Fatalf("expected 1 rapid transfer, got %d", own.Transfers)
	}
	if !own.HasFile(shaA) {
		t.Fatal("file should land in own drive after p2p transfer")
	}
}

func TestOwn115Step3SeedFallback(t *testing.T) {
	f, _ := buildOwn115Fixture(t)
	// 自有盘没有，播放记录库也没有 → 源盘兜底
	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d.Allowed {
		t.Fatalf("should be allowed: %v %+v", err, d)
	}
	defer d.Release()
	if d.Branch != engine.BranchSeedFallback {
		t.Fatalf("expected seed_fallback, got %s (%s)", d.Branch, d.Summary())
	}
	if !containsDetail(d.Steps, "rapid_transfer", "src=seed:115大") {
		t.Fatalf("trace should attribute seed source, got %s", d.Summary())
	}
}

func containsDetail(steps []engine.StepTrace, step, substr string) bool {
	for _, s := range steps {
		if s.Step == step && contains(s.Detail, substr) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// ---------------------------------------------------------------- 池模式全链路

func TestPoolProbeHit(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	if _, err := f.poolDrv["115小1"].RapidTransfer(context.Background(), shaA, "t", 1<<20); err != nil {
		t.Fatal(err)
	}
	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d.Allowed {
		t.Fatalf("should be allowed: %v %+v", err, d)
	}
	defer d.Release()
	if d.Branch != engine.BranchPoolProbeHit || d.AccountID != "115小1" {
		t.Fatalf("expected pool_probe_hit on 115小1, got %+v", d)
	}
}

func TestPoolShieldHitTransfer(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	// 神盾按 sha1 命中分享码；转存钩子把文件种进池盘（模拟转存生效）
	f.shield = memory.NewShieldClient(map[string]engine.ShieldResult{
		shaA: {Found: true, ShareCode: "abcd1234", Slug: "nextfind://res/1"},
	})
	poolDrv := map[string]engine.DriveClient{"115小1": f.poolDrv["115小1"]}
	f.shield.OnTransfer = func(slug, targetFolder string) {
		f.poolDrv["115小1"].RapidTransfer(context.Background(), shaA, "t", 1<<20)
	}
	f.eng = engine.New(engine.Config{Templates: map[string]int{"vip": 10}, Now: f.clock.now}, engine.Deps{
		Users: f.users, Pool: f.pool, Seed: memory.NewDriveClient("115大"),
		PoolDrv: poolDrv, Shield: f.shield, Records: f.records,
	})

	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d.Allowed {
		t.Fatalf("should be allowed: %v %+v", err, d)
	}
	defer d.Release()
	if d.Branch != engine.BranchPoolShieldHit {
		t.Fatalf("expected pool_shield_transfer, got %s (%s)", d.Branch, d.Summary())
	}
	if len(f.shield.Transfers) != 1 || f.shield.Transfers[0] != "nextfind://res/1" {
		t.Fatalf("expected one transfer of slug, got %v", f.shield.Transfers)
	}
	// 神盾转存成功后应建立 24h 锁：换文件再播仍走同一账号
	d2, _ := f.eng.Handle(context.Background(), req("u1", shaB))
	d2.Release()
	if d2.AccountID != "115小1" {
		t.Fatalf("lock should pin account, got %s", d2.AccountID)
	}
}

func TestPoolShieldMissSeedFallback(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	// 神盾查库未命中 → 后台主动搜索 + 源网盘秒传
	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d.Allowed {
		t.Fatalf("should be allowed: %v %+v", err, d)
	}
	defer d.Release()
	if d.Branch != engine.BranchPoolSeedFallback {
		t.Fatalf("expected pool_seed_fallback, got %s (%s)", d.Branch, d.Summary())
	}
	if len(f.shield.Background) != 1 || f.shield.Background[0] != "935597" {
		t.Fatalf("expected background search for tmdb 935597, got %v", f.shield.Background)
	}
	if f.poolDrv["115小1"].Transfers != 1 {
		t.Fatalf("expected 1 seed rapid transfer, got %d", f.poolDrv["115小1"].Transfers)
	}
	if !containsBranch(d.Steps, "shield_background_search", "triggered") {
		t.Fatalf("trace should show background search triggered, got %s", d.Summary())
	}
}

// ---------------------------------------------------------------- 统计与错误

func TestStatsSuccessAndFail(t *testing.T) {
	f := newFixture(engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	drv := f.poolDrv["115小1"]
	if _, err := drv.RapidTransfer(context.Background(), shaA, "t", 1<<20); err != nil {
		t.Fatal(err)
	}
	d, _ := f.eng.Handle(context.Background(), req("u1", shaA))
	d.Release()
	s, fl := f.eng.Stats("u1")
	if s != 1 || fl != 0 {
		t.Fatalf("expected (1,0), got (%d,%d)", s, fl)
	}
	// 注入取直链失败 → 记一次失败
	drv.FailURL = true
	f.clock.advance(11 * time.Minute) // 让缓存过期，走到取直链
	d2, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err == nil {
		t.Fatal("expected error on direct url failure")
	}
	if d2.Release != nil {
		d2.Release()
	}
	s, fl = f.eng.Stats("u1")
	if s != 1 || fl != 1 {
		t.Fatalf("expected (1,1), got (%d,%d)", s, fl)
	}
}

func TestUnknownUser(t *testing.T) {
	f := newFixture(engine.Config{},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	if _, err := f.eng.Handle(context.Background(), req("ghost", shaA)); err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestUnknownModeDenied(t *testing.T) {
	f := newFixture(engine.Config{},
		[]engine.User{{ID: "u1", Mode: "bogus", Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true}})
	d, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil {
		t.Fatalf("unknown mode should be a deny decision, not error: %v", err)
	}
	if d.Allowed || d.Branch != engine.BranchDeniedUnknownMode {
		t.Fatalf("expected denied_unknown_mode, got %+v", d)
	}
}
