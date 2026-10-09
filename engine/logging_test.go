package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"nextemby-replay/engine"
	"nextemby-replay/engine/memory"
)

// logSink 收集 LogFunc 的输出，供断言。存 "category|message"。
type logSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *logSink) fn() func(string, string) {
	return func(category, message string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.msgs = append(s.msgs, category+"|"+message)
	}
}

func (s *logSink) has(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

func (s *logSink) dump() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.msgs))
	copy(out, s.msgs)
	return out
}

// TestPoolSeedFallbackLogs 覆盖池模式源网盘秒传兜底的完整中文日志链，
// 并验证 UA 从请求透传到 Decision 与 DecisionSummary。
func TestPoolSeedFallbackLogs(t *testing.T) {
	f := newFixture(
		engine.Config{Templates: map[string]int{"vip": 10}, SeedAccountID: "115大"},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}})
	sink := &logSink{}
	f.eng.LogFunc = sink.fn()

	gb := float64(1024 * 1024 * 1024)
	size := int64(33.79 * gb) // → 33.79GB
	r := engine.PlaybackRequest{
		UserID: "u1", FileSHA1: shaA, FileName: "movie.mkv",
		FileSize: size, Client: "Filmly", TMDBID: "935597", MediaType: "movie",
		UA: "VidHub/2.1",
	}
	d, err := f.eng.Handle(context.Background(), r)
	if err != nil || !d.Allowed {
		t.Fatalf("Handle: %v %+v", err, d)
	}
	if d.Release != nil {
		d.Release()
	}
	if d.Branch != engine.BranchPoolSeedFallback {
		t.Fatalf("branch = %q", d.Branch)
	}

	want := []string{
		"play|收到请求 用户：u1(115小1) [Filmly] | 文件：movie.mkv | 大小：33.79GB",
		"play|正在执行全域 SHA1 探测 (115小1 | SHA1: AAAAAAAA...)",
		"play|[普通模式]全域 SHA1 探测失败，文件不存在(115小1 | SHA1: AAAAAAAA...)",
		"play|[普通模式]神盾快速查库未命中该资源 (文件：movie.mkv)",
		"play|正在尝试源网盘秒传：115大 -> u1(115小1)",
		"play|[普通模式]秒传成功：115大 -> u1(115小1) | 目录：/最近接收 | 文件：movie.mkv | 耗时：",
		"play|[普通模式]直连成功：u1 -> u1网盘 -> movie.mkv | 耗时：",
	}
	for _, w := range want {
		if !sink.has(w) {
			t.Errorf("missing log %q\ngot: %v", w, sink.dump())
		}
	}

	if d.UA != "VidHub/2.1" {
		t.Errorf("Decision.UA = %q", d.UA)
	}
	got := f.eng.RecentDecisions(1)
	if len(got) != 1 || got[0].UA != "VidHub/2.1" {
		t.Errorf("DecisionSummary.UA = %+v", got)
	}
}

// TestCacheHitLog 验证缓存命中日志。
func TestCacheHitLog(t *testing.T) {
	f := newFixture(
		engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}})
	sink := &logSink{}
	f.eng.LogFunc = sink.fn()

	doHandle(t, f, "u1", shaA) // 首次：秒传兜底，写缓存
	d := doHandle(t, f, "u1", shaA)
	if d.Branch != engine.BranchCacheHit {
		t.Fatalf("branch = %q", d.Branch)
	}
	if !sink.has("play|[普通模式]缓存命中直连：u1 -> 115小1 -> test.mkv") {
		t.Errorf("missing cache hit log\ngot: %v", sink.dump())
	}
}

// TestConcurrencyDenyLog 验证并发拒绝日志（error 分类）。
func TestConcurrencyDenyLog(t *testing.T) {
	f := newFixture(
		engine.Config{Templates: map[string]int{"vip": 1}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}})
	sink := &logSink{}
	f.eng.LogFunc = sink.fn()

	d1, err := f.eng.Handle(context.Background(), req("u1", shaA))
	if err != nil || !d1.Allowed {
		t.Fatalf("first: %v %+v", err, d1)
	}
	defer d1.Release()

	d2, err := f.eng.Handle(context.Background(), req("u1", shaB))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if d2.Allowed || d2.DenyReason != "concurrency_limit" {
		t.Fatalf("second should be denied: %+v", d2)
	}
	if !sink.has("error|[普通模式]并发拒绝 用户：u1 原因：concurrency_limit") {
		t.Errorf("missing deny log\ngot: %v", sink.dump())
	}
}

// TestDecisionErrorLog 验证决策异常日志（error 分类），且异常路径 UA 仍透传。
func TestDecisionErrorLog(t *testing.T) {
	f := newFixture(
		engine.Config{Templates: map[string]int{"vip": 10}},
		[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
		[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}})
	f.poolDrv["115小1"].FailProbe = true
	sink := &logSink{}
	f.eng.LogFunc = sink.fn()

	r := req("u1", shaA)
	r.UA = "TestAgent/1.0"
	d, err := f.eng.Handle(context.Background(), r)
	if err == nil {
		t.Fatalf("expected injected probe error")
	}
	if !sink.has("error|播放决策异常 用户：u1 文件：test.mkv 错误：") {
		t.Errorf("missing error log\ngot: %v", sink.dump())
	}
	if d.UA != "TestAgent/1.0" {
		t.Errorf("Decision.UA on error path = %q", d.UA)
	}
}

// TestOwn115Logs 验证自备网盘模式的中文日志（client 为空时显示 -）。
func TestOwn115Logs(t *testing.T) {
	f := newFixture(
		engine.Config{Templates: map[string]int{"vip": 10}, SeedAccountID: "115大"},
		[]engine.User{{ID: "u3", Mode: engine.ModeOwn115}},
		nil)
	f.eng.SetOwnDrive("u3", memory.NewDriveClient("u3的盘"))
	sink := &logSink{}
	f.eng.LogFunc = sink.fn()

	r := engine.PlaybackRequest{
		UserID: "u3", FileSHA1: shaC, FileName: "own.mkv",
		FileSize: 500 * 1024 * 1024, // → 500.00MB
		Client:   "",                // 空 client 应显示 -
	}
	d, err := f.eng.Handle(context.Background(), r)
	if err != nil || !d.Allowed {
		t.Fatalf("Handle: %v %+v", err, d)
	}
	if d.Release != nil {
		d.Release()
	}
	if d.Branch != engine.BranchSeedFallback {
		t.Fatalf("branch = %q", d.Branch)
	}

	want := []string{
		"play|收到请求 用户：u3(u3的盘) [-] | 文件：own.mkv | 大小：500.00MB",
		"play|正在执行自有盘 SHA1 探测 (u3的盘 | SHA1: CCCCCCCC...)",
		"play|[自备网盘模式]自有盘 SHA1 探测失败，文件不存在(u3的盘 | SHA1: CCCCCCCC...)",
		"play|正在尝试源网盘秒传：115大 -> u3(u3的盘)",
		"play|[自备网盘模式]秒传成功：115大 -> u3(u3的盘) | 目录：/最近接收 | 文件：own.mkv | 耗时：",
		"play|[自备网盘模式]直连成功：u3 -> u3网盘 -> own.mkv | 耗时：",
	}
	for _, w := range want {
		if !sink.has(w) {
			t.Errorf("missing log %q\ngot: %v", w, sink.dump())
		}
	}
}

// TestShieldHitLogs 验证神盾命中路径的中文日志。
func TestShieldHitLogs(t *testing.T) {
	clock := newFakeClock()
	poolDrv := memory.NewDriveClient("115小1")
	shield := memory.NewShieldClient(map[string]engine.ShieldResult{
		shaB: {Found: true, ShareCode: "ABC123", Slug: "nextfind://res/1"},
	})
	// 模拟转存生效：文件落盘，DirectURL 才能取到直链。
	shield.OnTransfer = func(slug, targetFolder string) {
		if _, err := poolDrv.RapidTransfer(context.Background(), shaB, "x", 1); err != nil {
			t.Errorf("seed transfer: %v", err)
		}
	}
	cfg := engine.Config{Templates: map[string]int{"vip": 10}, SeedAccountID: "115大"}
	cfg.Now = clock.now
	eng := engine.New(cfg, engine.Deps{
		Users:   memory.NewUserStore(engine.User{ID: "u1", Mode: engine.ModePool, Template: "vip"}),
		Pool:    memory.NewPoolStore(engine.PoolAccount{ID: "115小1", Healthy: true, MaxUsers: 4}),
		Seed:    memory.NewDriveClient("115大"),
		Own:     map[string]engine.DriveClient{},
		PoolDrv: map[string]engine.DriveClient{"115小1": poolDrv},
		Shield:  shield,
		Records: memory.NewPlaybackRecordStore(),
	})
	sink := &logSink{}
	eng.LogFunc = sink.fn()

	d, err := eng.Handle(context.Background(), req("u1", shaB))
	if err != nil || !d.Allowed {
		t.Fatalf("Handle: %v %+v", err, d)
	}
	if d.Release != nil {
		d.Release()
	}
	if d.Branch != engine.BranchPoolShieldHit {
		t.Fatalf("branch = %q", d.Branch)
	}
	for _, w := range []string{
		"play|[普通模式]全域 SHA1 探测失败，文件不存在(115小1 | SHA1: BBBBBBBB...)",
		"play|[普通模式]神盾快速查库命中该资源 (分享码：ABC123)",
		"play|[普通模式]直连成功：u1 -> u1网盘 -> test.mkv | 耗时：",
	} {
		if !sink.has(w) {
			t.Errorf("missing log %q\ngot: %v", w, sink.dump())
		}
	}
}

// TestTemplateOfResolution 验证 Config.TemplateOf 的解析语义：
// 非 nil 时优先于 Templates map；MaxConcurrent<=0 或未命中时回退 DefaultLimit。
func TestTemplateOfResolution(t *testing.T) {
	mk := func(cfg engine.Config) *fixture {
		return newFixture(cfg,
			[]engine.User{{ID: "u1", Mode: engine.ModePool, Template: "vip"}},
			[]engine.PoolAccount{{ID: "115小1", Healthy: true, MaxUsers: 4}})
	}
	ctx := context.Background()

	// 1) TemplateOf 命中且 MaxConcurrent=2：允许 2 个并发，第 3 个拒绝
	//    （Templates map 里的 100 应被覆盖）。
	f := mk(engine.Config{
		Templates:    map[string]int{"vip": 100},
		DefaultLimit: 1,
		TemplateOf: func(name string) (engine.Template, bool) {
			if name == "vip" {
				return engine.Template{Name: "vip", MaxConcurrent: 2, MaxDevices: 5}, true
			}
			return engine.Template{}, false
		},
	})
	d1, _ := f.eng.Handle(ctx, req("u1", shaA))
	d2, _ := f.eng.Handle(ctx, req("u1", shaB))
	d3, _ := f.eng.Handle(ctx, req("u1", shaC))
	if !d1.Allowed || !d2.Allowed {
		t.Fatalf("TemplateOf limit=2 should allow two sessions: %+v %+v", d1, d2)
	}
	if d3.Allowed || d3.DenyReason != "concurrency_limit" {
		t.Fatalf("third session should be denied: %+v", d3)
	}
	d1.Release()
	d2.Release()

	// 2) TemplateOf 未命中 → 回退 DefaultLimit。
	f2 := mk(engine.Config{
		DefaultLimit: 1,
		TemplateOf:   func(string) (engine.Template, bool) { return engine.Template{}, false },
	})
	e1, _ := f2.eng.Handle(ctx, req("u1", shaA))
	e2, _ := f2.eng.Handle(ctx, req("u1", shaB))
	if !e1.Allowed || e2.Allowed {
		t.Fatalf("miss should fall back to DefaultLimit=1: %+v %+v", e1, e2)
	}
	e1.Release()

	// 3) MaxConcurrent<=0 视为未配置 → 回退 DefaultLimit（不走 Templates map）。
	f3 := mk(engine.Config{
		Templates:    map[string]int{"vip": 100},
		DefaultLimit: 1,
		TemplateOf: func(string) (engine.Template, bool) {
			return engine.Template{Name: "vip", MaxConcurrent: 0}, true
		},
	})
	g1, _ := f3.eng.Handle(ctx, req("u1", shaA))
	g2, _ := f3.eng.Handle(ctx, req("u1", shaB))
	if !g1.Allowed || g2.Allowed {
		t.Fatalf("MaxConcurrent<=0 should fall back to DefaultLimit: %+v %+v", g1, g2)
	}
	g1.Release()
}
