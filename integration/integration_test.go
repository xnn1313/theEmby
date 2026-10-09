package integration

// integration_test.go — 三模块端到端联调（mock 版，零外部依赖）。
//
// 链路：fake 上游 Emby → gateway（PlaybackInfo 劫持）→ engine（决策）→
// memory 假 115 盘 → /nb/stream 302。全部断言在本地完成，不碰任何真实账号。
//
// 真实联调时把 wiring 里的 memory 实现换成 adapter115.New 的 Client 即可，
// 见 cmd/livetest。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nextemby-replay/engine"
	"nextemby-replay/engine/memory"
	"nextemby-replay/gateway"
)

const (
	testSHA1     = "A2375D8EA1B2C3D4E5F60718293A4B5C6D7E8F90"
	testEmbyPath = "/CloudNAS/CloudDrive/115open/剧集/去有风的地方/去有风的地方.Meet Yourself.2023.S01E04.mp4"
	testFileName = "去有风的地方.Meet Yourself.2023.S01E04.mp4"
)

// fakeFetcher 扮演 adapter115.Client.FileSHA1ByPath：
// 已知路径返回固定 SHA1，其余返回"不存在"。
type fakeFetcher struct{}

func (fakeFetcher) FileSHA1ByPath(_ context.Context, path115 string) (string, error) {
	if path115 == "/剧集/去有风的地方/去有风的地方.Meet Yourself.2023.S01E04.mp4" {
		return testSHA1, nil
	}
	return "", errNotFound
}

type notFoundError struct{}

func (notFoundError) Error() string { return "115: 文件不存在" }

var errNotFound = notFoundError{}

// wiring 是一套完整接线：engine + gateway + fake 上游。
type wiring struct {
	eng      *engine.Engine
	drv1     *memory.DriveClient // 115小1
	drv2     *memory.DriveClient // 115小2
	shield   *memory.ShieldClient
	gw       *httptest.Server
	upstream *httptest.Server
	// upstreamPath 是 fake 上游本次返回的 MediaSource.Path（按场景设置）。
	upstreamPath *string
}

// newWiring 搭一套新接线。seedPool=true 时把 testSHA1 预置进两个池盘（探测命中场景）。
func newWiring(t *testing.T, seedPool bool, shieldIndex map[string]engine.ShieldResult) *wiring {
	t.Helper()
	ctx := context.Background()

	w := &wiring{}
	embyPath := testEmbyPath
	w.upstreamPath = &embyPath

	// fake 上游 Emby：PlaybackInfo 一律返回单 MediaSource。
	w.upstream = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"MediaSources": []any{
				map[string]any{
					"Id":              "1",
					"Path":            *w.upstreamPath,
					"Name":            testFileName,
					"Size":            float64(5497558138),
					"DirectStreamUrl": "https://upstream.example/videos/1/stream",
				},
			},
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(doc)
	}))
	t.Cleanup(w.upstream.Close)

	drv1 := memory.NewDriveClient("115小1")
	drv2 := memory.NewDriveClient("115小2")
	if seedPool {
		drv1 = memory.NewDriveClient("115小1", testSHA1)
		drv2 = memory.NewDriveClient("115小2", testSHA1)
	}
	seed := memory.NewDriveClient("115大")
	w.drv1, w.drv2 = drv1, drv2

	shield := memory.NewShieldClient(shieldIndex)
	shield.OnTransfer = func(slug, targetFolder string) {
		// 模拟转存生效：文件落进两个池盘。
		drv1.RapidTransfer(ctx, testSHA1, testFileName, 5497558138)
		drv2.RapidTransfer(ctx, testSHA1, testFileName, 5497558138)
	}
	w.shield = shield

	eng := engine.New(
		engine.Config{Templates: map[string]int{"vip": 5, "limited": 1}},
		engine.Deps{
			Users: memory.NewUserStore(
				engine.User{ID: "lzy", Mode: engine.ModePool, Template: "vip"},
				engine.User{ID: "xrq", Mode: engine.ModePool, Template: "limited"},
			),
			Pool: memory.NewPoolStore(
				engine.PoolAccount{ID: "115小1", Healthy: true, MaxUsers: 4},
				engine.PoolAccount{ID: "115小2", Healthy: true, MaxUsers: 4},
			),
			Seed:    seed,
			PoolDrv: map[string]engine.DriveClient{"115小1": drv1, "115小2": drv2},
			Shield:  shield,
			Records: memory.NewPlaybackRecordStore(),
		},
	)
	w.eng = eng

	rules, err := gateway.ParsePathMap("/CloudNAS/CloudDrive/115open=/")
	if err != nil {
		t.Fatal(err)
	}
	finger := gateway.NewLiveFingerprintResolver(gateway.NewPathMapper(rules), fakeFetcher{})

	srv, err := gateway.NewServer(
		gateway.Config{
			Upstream: w.upstream.URL,
			APIKey:   "test-service-key",
			HMACKey:  []byte("0123456789abcdef0123456789abcdef"),
			PathMap:  "/CloudNAS/CloudDrive/115open=/",
		},
		eng,
		map[string]engine.DriveClient{"115大": seed, "115小1": drv1, "115小2": drv2},
		finger,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	w.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(w.gw.Close)
	return w
}

// postPlaybackInfo 发一次劫持请求，返回解析后的 MediaSources。
func postPlaybackInfo(t *testing.T, w *wiring, userID, embyDevice string) (int, []map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.gw.URL+"/emby/Items/42/PlaybackInfo?api_key=x", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NB-User", userID)
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Emby", Device="`+embyDevice+`", DeviceId="d1", Version="4.8"`)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, body)
	}
	raw, _ := doc["MediaSources"].([]any)
	var sources []map[string]any
	for _, rs := range raw {
		if m, ok := rs.(map[string]any); ok {
			sources = append(sources, m)
		}
	}
	return resp.StatusCode, sources
}

// getNoRedirect 发 GET 但不跟随跳转，返回状态码与 Location。
func getNoRedirect(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// TestPoolProbeHitFullChain 池模式探测命中：PlaybackInfo 改写 → /nb/stream 302 到 115 直链。
func TestPoolProbeHitFullChain(t *testing.T) {
	w := newWiring(t, true, nil)

	status, sources := postPlaybackInfo(t, w, "lzy", "Filmly")
	if status != 200 {
		t.Fatalf("PlaybackInfo 状态码 = %d, 期望 200", status)
	}
	if len(sources) == 0 {
		t.Fatal("上游没有返回 MediaSources")
	}
	streamURL, _ := sources[0]["DirectStreamUrl"].(string)
	if !strings.HasPrefix(streamURL, "/nb/stream?t=") {
		t.Fatalf("DirectStreamUrl 未被改写为 /nb/stream: %q", streamURL)
	}

	code, loc := getNoRedirect(t, w.gw.URL+streamURL)
	if code != http.StatusFound {
		t.Fatalf("/nb/stream 状态码 = %d, 期望 302", code)
	}
	// 注：http.Redirect 会对中文账号名做 percent-encoding，先解码再断言。
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("302 Location 解析失败: %v (%q)", err, loc)
	}
	if !strings.HasPrefix(parsed.Path, "/d/115小") || !strings.HasSuffix(parsed.Path, "/"+testSHA1) {
		t.Fatalf("302 目标不是 115 直链: %q（解码后 path=%q）", loc, parsed.Path)
	}

	// 引擎统计：lzy 直连成功计数应为 1。
	if ok, fail := w.eng.Stats("lzy"); ok != 1 || fail != 0 {
		t.Fatalf("lzy 统计 = 成功 %d 失败 %d, 期望 1/0", ok, fail)
	}
}

// TestEngineCacheAndLock engine 层：24h 锁粘性 + 10min 缓存命中（零 115 API）。
func TestEngineCacheAndLock(t *testing.T) {
	w := newWiring(t, true, nil)
	ctx := context.Background()

	req := engine.PlaybackRequest{
		UserID: "lzy", FileSHA1: testSHA1, FileName: testFileName,
		FileSize: 5497558138, Client: "Filmly",
	}
	d1, err := w.eng.Handle(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !d1.Allowed || d1.Branch != engine.BranchPoolProbeHit {
		t.Fatalf("首次决策 = allowed=%v branch=%s, 期望 pool_probe_hit", d1.Allowed, d1.Branch)
	}
	acct := d1.AccountID
	if acct != "115小1" && acct != "115小2" {
		t.Fatalf("服务账号 = %q, 期望池账号", acct)
	}

	probesBefore := w.drv1.Probes + w.drv2.Probes
	urlsBefore := w.drv1.DirectURLs + w.drv2.DirectURLs

	d2, err := w.eng.Handle(ctx, req) // 10 分钟内重播
	if err != nil {
		t.Fatal(err)
	}
	if d2.Branch != engine.BranchCacheHit {
		t.Fatalf("重播分支 = %s, 期望 cache_hit", d2.Branch)
	}
	if d2.AccountID != acct {
		t.Fatalf("重播账号 = %q, 期望与首次一致 %q（24h 锁粘性）", d2.AccountID, acct)
	}
	if d2.DirectURL != d1.DirectURL {
		t.Fatal("缓存命中应返回同一条直链")
	}
	if got := w.drv1.Probes + w.drv2.Probes; got != probesBefore {
		t.Fatalf("缓存命中后又打了 %d 次探测 API, 期望 0", got-probesBefore)
	}
	if got := w.drv1.DirectURLs + w.drv2.DirectURLs; got != urlsBefore {
		t.Fatalf("缓存命中后又取了 %d 次直链, 期望 0", got-urlsBefore)
	}
	d1.Release()
	d2.Release()
}

// TestPoolShieldHitBranch engine 层：探测未命中 → 神盾命中 → 转存进池。
func TestPoolShieldHitBranch(t *testing.T) {
	w := newWiring(t, false, map[string]engine.ShieldResult{
		testSHA1: {Found: true, ShareCode: "abcd1234", Slug: "nextfind://res/1"},
	})
	ctx := context.Background()

	d, err := w.eng.Handle(ctx, engine.PlaybackRequest{
		UserID: "lzy", FileSHA1: testSHA1, FileName: testFileName,
		FileSize: 5497558138, Client: "VidHub", TMDBID: "935597", MediaType: "movie",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	if !d.Allowed || d.Branch != engine.BranchPoolShieldHit {
		t.Fatalf("决策 = allowed=%v branch=%s, 期望 pool_shield_transfer", d.Allowed, d.Branch)
	}
	if len(w.shield.Transfers) != 1 || w.shield.Transfers[0] != "nextfind://res/1" {
		t.Fatalf("神盾转存未被调用: %v", w.shield.Transfers)
	}
	if !strings.HasPrefix(d.DirectURL, "https://115.example/d/") {
		t.Fatalf("直链异常: %q", d.DirectURL)
	}
}

// TestPoolSeedFallbackBranch engine 层：探测未命中 → 神盾未命中 → 源盘秒传兜底。
func TestPoolSeedFallbackBranch(t *testing.T) {
	w := newWiring(t, false, nil) // 池盘无文件，神盾索引为空
	ctx := context.Background()

	d, err := w.eng.Handle(ctx, engine.PlaybackRequest{
		UserID: "lzy", FileSHA1: testSHA1, FileName: testFileName,
		FileSize: 5497558138, Client: "Filmly",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	if !d.Allowed || d.Branch != engine.BranchPoolSeedFallback {
		t.Fatalf("决策 = allowed=%v branch=%s, 期望 pool_seed_fallback", d.Allowed, d.Branch)
	}
	// 秒传应落盘到该账号的 /最近接收。
	var drv *memory.DriveClient
	if d.AccountID == "115小1" {
		drv = w.drv1
	} else {
		drv = w.drv2
	}
	if !drv.HasFile(testSHA1) {
		t.Fatalf("秒传后 %s 盘里没有该文件", d.AccountID)
	}
}

// TestGracefulDegradation 路径映射 miss → 优雅降级：上游响应原样返回。
func TestGracefulDegradation(t *testing.T) {
	w := newWiring(t, true, nil)
	other := "/mnt/nas/电影/狂蟒惊魂.mkv"
	w.upstreamPath = &other

	status, sources := postPlaybackInfo(t, w, "lzy", "Filmly")
	if status != 200 {
		t.Fatalf("状态码 = %d, 期望 200（降级透传）", status)
	}
	u, _ := sources[0]["DirectStreamUrl"].(string)
	if u != "https://upstream.example/videos/1/stream" {
		t.Fatalf("降级时 DirectStreamUrl 应保持上游原样, 得到 %q", u)
	}
}

// TestConcurrencyDenied 并发超限 → 429。
func TestConcurrencyDenied(t *testing.T) {
	w := newWiring(t, true, nil)

	status, _ := postPlaybackInfo(t, w, "xrq", "VidHub") // limited 模板，上限 1
	if status != 200 {
		t.Fatalf("首次播放状态码 = %d, 期望 200", status)
	}
	// 会话未释放（网关按 token 过期才释放），第二次应被拒绝。
	status2, _ := postPlaybackInfo(t, w, "xrq", "VidHub")
	if status2 != http.StatusTooManyRequests {
		t.Fatalf("超限播放状态码 = %d, 期望 429", status2)
	}
}

// TestStreamTokenForged 伪造 token → 403。
func TestStreamTokenForged(t *testing.T) {
	w := newWiring(t, true, nil)
	code, _ := getNoRedirect(t, w.gw.URL+"/nb/stream?t=forged-token")
	if code != http.StatusForbidden {
		t.Fatalf("伪造 token 状态码 = %d, 期望 403", code)
	}
}

// TestTimingSanity 各环节耗时量级：纯内存链路应在 100ms 内完成。
func TestTimingSanity(t *testing.T) {
	w := newWiring(t, true, nil)
	start := time.Now()
	status, sources := postPlaybackInfo(t, w, "lzy", "Filmly")
	if status != 200 || len(sources) == 0 {
		t.Fatal("PlaybackInfo 失败")
	}
	if _, _, err := func() (int, string, error) {
		c, l := getNoRedirect(t, w.gw.URL+sources[0]["DirectStreamUrl"].(string))
		return c, l, nil
	}(); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("内存链路耗时 %v, 期望 < 100ms（真实链路约 1.8s：秒传 1.4s + 直链 0.3s）", el)
	}
}
