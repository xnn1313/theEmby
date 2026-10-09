package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nextemby-replay/engine"
	emem "nextemby-replay/engine/memory"
)

// webFixture 是管理后台/个人中心测试的装配：内存引擎 + webUI 已启用。
type webFixture struct {
	gw     *httptest.Server
	rec    *upstreamRecorder
	logBuf *bytes.Buffer
	eng    *engine.Engine
}

func newWebFixture(t *testing.T) *webFixture {
	t.Helper()

	rec := &upstreamRecorder{}
	up := httptest.NewServer(rec)
	t.Cleanup(up.Close)

	logBuf := &bytes.Buffer{}
	logger := log.New(logBuf, "[test] ", 0)

	users := emem.NewUserStore(
		engine.User{ID: "lzy", Mode: engine.ModePool, Template: "vip"},
		engine.User{ID: "guest", Mode: engine.ModePool},
	)
	pool := emem.NewPoolStore(engine.PoolAccount{ID: "115小1", Healthy: true, MaxUsers: 4})
	d1 := emem.NewDriveClient("115小1")
	seed := emem.NewDriveClient("115大")
	eng := engine.New(
		engine.Config{Templates: map[string]int{"vip": 5}, DefaultLimit: 1},
		engine.Deps{
			Users:   users,
			Pool:    pool,
			Seed:    seed,
			PoolDrv: map[string]engine.DriveClient{"115小1": d1},
			Shield:  emem.NewShieldClient(map[string]engine.ShieldResult{}),
			Records: emem.NewPlaybackRecordStore(),
		},
	)
	drives := map[string]engine.DriveClient{"115大": seed, "115小1": d1}
	rules, err := ParsePathMap("/emby/media=/115/媒体库")
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeFetcher{}
	fetcher.seed("/115/媒体库/去有风的地方.S01E04.mp4", testSHA1)
	finger := NewLiveFingerprintResolver(NewPathMapper(rules), fetcher)

	cfg := Config{Upstream: up.URL, APIKey: testAPIKey, HMACKey: []byte(testHMACKey),
		Listen: ":8091", PathMap: "/CloudNAS/CloudDrive/115open=/"}
	srv, err := NewServer(cfg, eng, drives, finger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := NewMemoryCookieStore([]byte("0123456789abcdef0123456789abcdef"))
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

	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	return &webFixture{gw: gw, rec: rec, logBuf: logBuf, eng: eng}
}

func adminGet(t *testing.T, fx *webFixture, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", fx.gw.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestAdminAuth(t *testing.T) {
	fx := newWebFixture(t)
	// 无 token → 401
	if resp := adminGet(t, fx, "/nb/admin/api/stats", ""); resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("want 401, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 错误 token → 401
	if resp := adminGet(t, fx, "/nb/admin/api/stats", "wrong"); resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("want 401, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// ?token= 方式 → 200
	resp, err := http.Get(fx.gw.URL + "/nb/admin/api/stats?token=test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("query token: want 200, got %d", resp.StatusCode)
	}
	// Bearer 头 → 200
	if resp := adminGet(t, fx, "/nb/admin/api/stats", "test-admin-token"); resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("bearer: want 200, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 页面可访问
	resp, err = http.Get(fx.gw.URL + "/nb/admin/?token=test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("admin page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func postPlayback(t *testing.T, fx *webFixture, nbUser string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", fx.gw.URL+"/emby/Items/x/PlaybackInfo?nb_user="+nbUser,
		strings.NewReader(`{"UserId":"u"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdminStatsAndDecisions(t *testing.T) {
	fx := newWebFixture(t)
	resp := postPlayback(t, fx, "lzy")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("playback: %d", resp.StatusCode)
	}

	var stats struct {
		Branches     map[string]int `json:"branches"`
		TotalToday   int            `json:"totalToday"`
		CacheHitRate float64        `json:"cacheHitRate"`
	}
	r := adminGet(t, fx, "/nb/admin/api/stats", "test-admin-token")
	decodeJSON(t, r, &stats)
	if stats.TotalToday != 1 || stats.Branches[engine.BranchPoolSeedFallback] != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	// 第二次同文件 → 缓存命中
	resp = postPlayback(t, fx, "lzy")
	resp.Body.Close()
	r = adminGet(t, fx, "/nb/admin/api/stats", "test-admin-token")
	decodeJSON(t, r, &stats)
	if stats.Branches[engine.BranchCacheHit] != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.CacheHitRate != 0.5 {
		t.Fatalf("hitRate = %v", stats.CacheHitRate)
	}

	var decs []decisionJSON
	r = adminGet(t, fx, "/nb/admin/api/decisions?limit=1", "test-admin-token")
	decodeJSON(t, r, &decs)
	if len(decs) != 1 || decs[0].UserID != "lzy" || decs[0].ElapsedMs < 0 {
		t.Fatalf("decisions = %+v", decs)
	}
}

func TestAdminUsersCRUD(t *testing.T) {
	fx := newWebFixture(t)
	var users []adminUserJSON
	r := adminGet(t, fx, "/nb/admin/api/users", "test-admin-token")
	decodeJSON(t, r, &users)
	if len(users) != 2 || users[0].ID != "guest" || users[1].ID != "lzy" {
		t.Fatalf("users = %+v", users)
	}

	// 更新模式与模板
	req, _ := http.NewRequest("PUT", fx.gw.URL+"/nb/admin/api/users/lzy?token=test-admin-token",
		strings.NewReader(`{"mode":"115","template":"vip"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	r = adminGet(t, fx, "/nb/admin/api/users", "test-admin-token")
	decodeJSON(t, r, &users)
	var lzy *adminUserJSON
	for i := range users {
		if users[i].ID == "lzy" {
			lzy = &users[i]
		}
	}
	if lzy == nil || lzy.Mode != "115" {
		t.Fatalf("users = %+v", users)
	}

	// 非法模式 → 400
	req, _ = http.NewRequest("PUT", fx.gw.URL+"/nb/admin/api/users/lzy?token=test-admin-token",
		strings.NewReader(`{"mode":"s3","template":"vip"}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	// 未知用户 → 404
	req, _ = http.NewRequest("PUT", fx.gw.URL+"/nb/admin/api/users/nobody?token=test-admin-token",
		strings.NewReader(`{"mode":"pool","template":"vip"}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestAdminPoolHealth(t *testing.T) {
	fx := newWebFixture(t)
	var pool []poolStatusJSON
	r := adminGet(t, fx, "/nb/admin/api/pool", "test-admin-token")
	decodeJSON(t, r, &pool)
	if len(pool) != 1 || !pool[0].Healthy || pool[0].MaxUsers != 4 {
		t.Fatalf("pool = %+v", pool)
	}

	req, _ := http.NewRequest("POST", fx.gw.URL+"/nb/admin/api/pool/115小1/health?token=test-admin-token",
		strings.NewReader(`{"healthy":false}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	r = adminGet(t, fx, "/nb/admin/api/pool", "test-admin-token")
	decodeJSON(t, r, &pool)
	if pool[0].Healthy {
		t.Fatalf("pool = %+v", pool)
	}

	// 未知账号 → 404
	req, _ = http.NewRequest("POST", fx.gw.URL+"/nb/admin/api/pool/nope/health?token=test-admin-token",
		strings.NewReader(`{"healthy":true}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestAdminConfigNoSecrets(t *testing.T) {
	fx := newWebFixture(t)
	r := adminGet(t, fx, "/nb/admin/api/config", "test-admin-token")
	var cfg map[string]string
	decodeJSON(t, r, &cfg)
	if cfg["upstream"] == "" || cfg["listen"] == "" {
		t.Fatalf("cfg = %+v", cfg)
	}
	for k, v := range cfg {
		if strings.Contains(v, testAPIKey) {
			t.Fatalf("secret leaked in config %s", k)
		}
	}
}
