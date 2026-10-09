package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"nextemby-replay/engine"
	emem "nextemby-replay/engine/memory"
)

// testAPIKey 是测试专用假 Key，绝不使用真实 Key。
const testAPIKey = "test-api-key-123"

const testHMACKey = "test-hmac-key-0123456789abcdef"

const (
	testEmbyPath = "/emby/media/去有风的地方.S01E04.mp4"
	testPath115  = "/115/媒体库/去有风的地方.S01E04.mp4"
	testSHA1     = "A2375D8E0000000000000000000000000000000001"
)

// upstreamRecorder 记录打到 mock 上游的请求，并按路径返回固定响应。
type upstreamRecorder struct {
	mu          sync.Mutex
	method      string
	path        string
	query       url.Values
	nbUserHdr   string
	playbackCnt int
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.method = r.Method
	u.path = r.URL.Path
	u.query = r.URL.Query()
	u.nbUserHdr = r.Header.Get("X-NB-User")
	if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
		u.playbackCnt++
	}
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") && r.Method == http.MethodPost {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"MediaSources": []any{
				map[string]any{
					"Id":              "ms1",
					"Path":            testEmbyPath,
					"Name":            "去有风的地方.Meet Yourself.2023.S01E04.mp4",
					"Size":            float64(5497558138),
					"DirectStreamUrl": "https://upstream.example/videos/42/stream?tok=abc",
				},
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"echo": r.URL.Path})
}

func (u *upstreamRecorder) last() (method, path string, query url.Values, nbUser string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.method, u.path, u.query, u.nbUserHdr
}

type fixture struct {
	gw      *httptest.Server
	rec     *upstreamRecorder
	logBuf  *bytes.Buffer
	fetcher *fakeFetcher
	finger  *LiveFingerprintResolver
	hmacKey []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	rec := &upstreamRecorder{}
	up := httptest.NewServer(rec)
	t.Cleanup(up.Close)

	logBuf := &bytes.Buffer{}
	logger := log.New(logBuf, "[test] ", 0)

	users := emem.NewUserStore(
		engine.User{ID: "lzy", Mode: engine.ModePool, Template: "vip"},
		engine.User{ID: "guest", Mode: engine.ModePool},
		engine.User{ID: "limited", Mode: engine.ModePool, Template: "tiny"},
	)
	pool := emem.NewPoolStore(engine.PoolAccount{ID: "115小1", Healthy: true, MaxUsers: 4})
	d1 := emem.NewDriveClient("115小1")
	seed := emem.NewDriveClient("115大")
	drives := map[string]engine.DriveClient{"115大": seed, "115小1": d1}
	eng := engine.New(
		engine.Config{Templates: map[string]int{"vip": 5, "tiny": 1}, DefaultLimit: 1},
		engine.Deps{
			Users:   users,
			Pool:    pool,
			Seed:    seed,
			PoolDrv: map[string]engine.DriveClient{"115小1": d1},
			Shield:  emem.NewShieldClient(map[string]engine.ShieldResult{}),
			Records: emem.NewPlaybackRecordStore(),
		},
	)
	rules, err := ParsePathMap("/emby/media=/115/媒体库")
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeFetcher{}
	finger := NewLiveFingerprintResolver(NewPathMapper(rules), fetcher)

	cfg := Config{Upstream: up.URL, APIKey: testAPIKey, HMACKey: []byte(testHMACKey)}
	srv, err := NewServer(cfg, eng, drives, finger, logger)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	return &fixture{gw: gw, rec: rec, logBuf: logBuf, fetcher: fetcher, finger: finger, hmacKey: []byte(testHMACKey)}
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func postPlaybackInfo(t *testing.T, gwURL, user string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gwURL+"/emby/Items/42/PlaybackInfo?nb_user="+user, strings.NewReader(`{"UserId":"u1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NB-User", user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func firstDirectStreamURL(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("解析 PlaybackInfo 响应失败: %v", err)
	}
	srcs, ok := doc["MediaSources"].([]any)
	if !ok || len(srcs) == 0 {
		t.Fatal("响应中没有 MediaSources")
	}
	u, _ := srcs[0].(map[string]any)["DirectStreamUrl"].(string)
	return u
}

func TestPassthroughKeepsPathAndQuery(t *testing.T) {
	fx := newFixture(t)
	resp, err := http.Get(fx.gw.URL + "/emby/System/Info?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	method, path, query, _ := fx.rec.last()
	if method != "GET" || path != "/emby/System/Info" {
		t.Fatalf("上游收到 %s %s", method, path)
	}
	if query.Get("x") != "1" {
		t.Fatalf("query x 丢失: %v", query)
	}
	if query.Get("api_key") != testAPIKey {
		t.Fatalf("api_key 未补服务 Key: %v", query)
	}
}

func TestPassthroughKeepsClientAPIKey(t *testing.T) {
	fx := newFixture(t)
	resp, err := http.Get(fx.gw.URL + "/emby/Users?api_key=clientkey")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, _, query, _ := fx.rec.last()
	if query.Get("api_key") != "clientkey" {
		t.Fatalf("客户端自带 key 被覆盖: %v", query)
	}
}

func TestPassthroughStripsNBUser(t *testing.T) {
	fx := newFixture(t)
	req, _ := http.NewRequest(http.MethodGet, fx.gw.URL+"/emby/System/Info", nil)
	req.Header.Set("X-NB-User", "lzy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, _, _, nbUser := fx.rec.last()
	if nbUser != "" {
		t.Fatalf("X-NB-User 泄漏到上游: %q", nbUser)
	}
}

func TestPlaybackInfoHijackAndStream(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.seed(testPath115, testSHA1)

	resp := postPlaybackInfo(t, fx.gw.URL, "lzy")
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	streamURL := firstDirectStreamURL(t, resp)
	if !strings.HasPrefix(streamURL, "/nb/stream?t=") {
		t.Fatalf("DirectStreamUrl 未被改写: %s", streamURL)
	}

	// 跟随 /nb/stream：应 302 到 115 直链。
	token := strings.TrimPrefix(streamURL, "/nb/stream?t=")
	r2, err := noRedirectClient().Get(fx.gw.URL + "/nb/stream?t=" + token)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusFound {
		t.Fatalf("stream 状态码 = %d", r2.StatusCode)
	}
	want := "/d/115小1/" + testSHA1
	loc, err := url.Parse(r2.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != want {
		t.Fatalf("302 Location path = %q, want %q", loc.Path, want)
	}

	// 日志里绝不能出现 API Key。
	if strings.Contains(fx.logBuf.String(), testAPIKey) {
		t.Fatalf("日志泄漏了 API Key:\n%s", fx.logBuf.String())
	}
}

func TestPlaybackInfoNoEmbyPrefix(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.seed(testPath115, testSHA1)
	req, _ := http.NewRequest(http.MethodPost, fx.gw.URL+"/Videos/7/PlaybackInfo", strings.NewReader(`{}`))
	req.Header.Set("X-NB-User", "lzy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if u := firstDirectStreamURL(t, resp); !strings.HasPrefix(u, "/nb/stream?t=") {
		t.Fatalf("无 /emby 前缀的路径未被劫持: %s", u)
	}
}

func TestFingerprintMissPassthrough(t *testing.T) {
	fx := newFixture(t) // 快照为空，指纹查不到
	resp := postPlaybackInfo(t, fx.gw.URL, "lzy")
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	u := firstDirectStreamURL(t, resp)
	want := "https://upstream.example/videos/42/stream?tok=abc"
	if u != want {
		t.Fatalf("指纹未命中时应原样透传，得到 %s", u)
	}
}

func TestStreamTokenInvalid(t *testing.T) {
	fx := newFixture(t)
	c := noRedirectClient()
	for name, token := range map[string]string{
		"empty":   "",
		"garbage": "not-a-token",
		"tamper":  "abc.def",
	} {
		r, err := c.Get(fx.gw.URL + "/nb/stream?t=" + token)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: 状态码 = %d, want 403", name, r.StatusCode)
		}
	}
}

func TestStreamTokenExpired(t *testing.T) {
	fx := newFixture(t)
	// 手工签发一个已过期的 token。
	raw, _ := json.Marshal(streamClaims{Exp: time.Now().Add(-time.Hour).Unix(), Acct: "115小1", SHA1: testSHA1})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, fx.hmacKey)
	mac.Write([]byte(payload))
	token := payload + "." + hex.EncodeToString(mac.Sum(nil))

	r, err := noRedirectClient().Get(fx.gw.URL + "/nb/stream?t=" + token)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("过期 token 状态码 = %d, want 403", r.StatusCode)
	}
}

func TestStreamRefetchOnCacheMiss(t *testing.T) {
	fx := newFixture(t)
	// 不经过 PlaybackInfo，直接签发合法 token：网关缓存未命中 → 经 DriveClient 现取。
	token := issueStreamToken(fx.hmacKey, "115小1", testSHA1, time.Now())
	// memory drive 里还没有这个文件：先手动秒传进去（模拟池里已有）。
	// 这里直接用快照+引擎走一遍更简单：先调一次 PlaybackInfo 建立文件。
	fx.fetcher.seed(testPath115, testSHA1)
	resp := postPlaybackInfo(t, fx.gw.URL, "guest")
	resp.Body.Close()

	r, err := noRedirectClient().Get(fx.gw.URL + "/nb/stream?t=" + token)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d, want 302", r.StatusCode)
	}
	want := "/d/115小1/" + testSHA1
	loc, err := url.Parse(r.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != want {
		t.Fatalf("Location path = %q, want %q", loc.Path, want)
	}
}

func TestConcurrencyDenied(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.seed(testPath115, testSHA1)

	r1 := postPlaybackInfo(t, fx.gw.URL, "limited") // tiny 模板上限 1，占用会话
	r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("第一次状态码 = %d", r1.StatusCode)
	}
	r2 := postPlaybackInfo(t, fx.gw.URL, "limited")
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第二次状态码 = %d, want 429", r2.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(r2.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "concurrency_limit" {
		t.Fatalf("body = %v", body)
	}
}

func TestTokenRoundtrip(t *testing.T) {
	key := []byte("k")
	tok := issueStreamToken(key, "115小2", "ABC", time.Now())
	c, err := verifyStreamToken(key, tok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.Acct != "115小2" || c.SHA1 != "ABC" {
		t.Fatalf("claims = %+v", c)
	}
	if _, err := verifyStreamToken([]byte("wrong"), tok, time.Now()); err == nil {
		t.Fatal("错 key 应验签失败")
	}
	if _, err := verifyStreamToken(key, tok, time.Now().Add(3*time.Hour)); err == nil {
		t.Fatal("过期应验签失败")
	}
}

func TestParsePathMap(t *testing.T) {
	rules, err := ParsePathMap("/emby/media=/115/媒体库;/mnt/tv=/115/剧集")
	if err != nil {
		t.Fatal(err)
	}
	m := NewPathMapper(rules)
	if got, ok := m.Map115("/emby/media/a.mp4"); !ok || got != "/115/媒体库/a.mp4" {
		t.Fatalf("got %q %v", got, ok)
	}
	if got, ok := m.Map115("/mnt/tv/b.mkv"); !ok || got != "/115/剧集/b.mkv" {
		t.Fatalf("got %q %v", got, ok)
	}
	// 前缀边界：/emby/media2 不应命中 /emby/media
	if _, ok := m.Map115("/emby/media2/x.mp4"); ok {
		t.Fatal("前缀边界误命中")
	}
	if _, ok := m.Map115("/other/x.mp4"); ok {
		t.Fatal("无规则路径应 miss")
	}
	// 最长前缀优先
	rules2, _ := ParsePathMap("/a=/x;/a/b=/y")
	m2 := NewPathMapper(rules2)
	if got, _ := m2.Map115("/a/b/c"); got != "/y/c" {
		t.Fatalf("最长前缀未优先: %q", got)
	}
	if _, err := ParsePathMap("noequals"); err == nil {
		t.Fatal("非法格式应报错")
	}
	if rules, err := ParsePathMap(""); err != nil || rules != nil {
		t.Fatalf("空字符串应返回 nil, err=%v", err)
	}
}

// fakeFetcher 模拟 adapter115.Client.FileSHA1ByPath（按 115 路径实时取 sha1）。
type fakeFetcher struct {
	mu    sync.Mutex
	index map[string]string
	calls int
}

func (f *fakeFetcher) seed(path115, sha1 string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.index == nil {
		f.index = map[string]string{}
	}
	f.index[path115] = sha1
}

func (f *fakeFetcher) FileSHA1ByPath(_ context.Context, path115 string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	s, ok := f.index[path115]
	if !ok {
		return "", fmt.Errorf("not found: %s", path115)
	}
	return s, nil
}

func TestLiveResolver(t *testing.T) {
	ctx := context.Background()
	rules, _ := ParsePathMap("/emby/media=/115/媒体库")
	fetcher := &fakeFetcher{}
	fetcher.seed("/115/媒体库/new.mp4", "NEWHASH")
	r := NewLiveFingerprintResolver(NewPathMapper(rules), fetcher)

	// 映射命中 + 实时取命中
	sha1, ok := r.ResolveSHA1(ctx, "/emby/media/new.mp4")
	if !ok || sha1 != "NEWHASH" {
		t.Fatalf("got %q %v", sha1, ok)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d", fetcher.calls)
	}
	// 每次都实时取（无快照）：第二次同样调 fetcher
	if _, ok := r.ResolveSHA1(ctx, "/emby/media/new.mp4"); !ok || fetcher.calls != 2 {
		t.Fatal("应每次实时取 sha1")
	}
	// fetcher miss → false（网关优雅降级）
	if _, ok := r.ResolveSHA1(ctx, "/emby/media/unknown.mp4"); ok {
		t.Fatal("应返回 miss")
	}
	// 无映射规则 → false（不调 fetcher）
	if _, ok := r.ResolveSHA1(ctx, "/nomap/x.mp4"); ok {
		t.Fatal("无映射应 miss")
	}
	// fetcher 为 nil → 恒 miss
	r2 := NewLiveFingerprintResolver(NewPathMapper(rules), nil)
	if _, ok := r2.ResolveSHA1(ctx, "/emby/media/new.mp4"); ok {
		t.Fatal("无 fetcher 应 miss")
	}
}

func TestLiveResolverRealMapping(t *testing.T) {
	// nextemby 真实映射：Emby 路径中的 /CloudNAS/CloudDrive/115open 对应 115 根目录。
	ctx := context.Background()
	rules, err := ParsePathMap("/CloudNAS/CloudDrive/115open=/")
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeFetcher{}
	fetcher.seed("/电影/去有风的地方.S01E04.mp4", "A2375D8E")
	r := NewLiveFingerprintResolver(NewPathMapper(rules), fetcher)
	sha1, ok := r.ResolveSHA1(ctx, "/CloudNAS/CloudDrive/115open/电影/去有风的地方.S01E04.mp4")
	if !ok || sha1 != "A2375D8E" {
		t.Fatalf("got %q %v", sha1, ok)
	}
}
