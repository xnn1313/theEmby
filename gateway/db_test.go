package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nextemby-replay/engine"
	emem "nextemby-replay/engine/memory"
	"nextemby-replay/store"
)

var testCookieKey = []byte("0123456789abcdef0123456789abcdef")

// newDBFixture 搭一个接了真实 SQLite 的网关：engine 的 Users/Records/Pool
// 走 store，admin/me 的统计与日志读 DB。
func newDBFixture(t *testing.T) (*httptest.Server, *store.Store, *engine.Engine) {
	t.Helper()
	ctx := context.Background()

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
	cfg := Config{Upstream: "http://127.0.0.1:1", APIKey: testAPIKey,
		HMACKey: []byte(testHMACKey), Listen: ":8091"}
	srv, err := NewServer(cfg, eng,
		map[string]engine.DriveClient{"115大": seed, "115小1": d1},
		NewLiveFingerprintResolver(NewPathMapper(nil), nil), logger)
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

	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	return gw, st, eng
}

func dbAdminGet(t *testing.T, gwURL, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", gwURL+path, nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestDBDecisionsFlow：一次真实 engine.Handle → OnDecision 落库 →
// admin decisions/stats 读 DB。
func TestDBDecisionsFlow(t *testing.T) {
	gw, st, eng := newDBFixture(t)
	ctx := context.Background()

	// lzy 池模式播一次：115小1 内存盘里没有该文件 → 探测 miss → 神盾空 → 源盘秒传
	d, err := eng.Handle(ctx, engine.PlaybackRequest{
		UserID: "lzy", FileSHA1: "AAAABBBB", FileName: "t.mp4", FileSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Release != nil {
		d.Release()
	}
	if !d.Allowed {
		t.Fatalf("decision: %+v", d)
	}

	// decisions 接口应读到这一条（DB）
	var decs []decisionJSON
	decodeJSON(t, dbAdminGet(t, gw.URL, "/nb/admin/api/decisions?limit=10"), &decs)
	if len(decs) != 1 || decs[0].UserID != "lzy" {
		t.Fatalf("decisions: %+v", decs)
	}
	// stats 聚合
	var stats map[string]any
	decodeJSON(t, dbAdminGet(t, gw.URL, "/nb/admin/api/stats"), &stats)
	if stats["totalToday"].(float64) != 1 {
		t.Fatalf("stats: %v", stats)
	}
	// 再播一次：直链缓存命中（内存），同样落库
	d2, err := eng.Handle(ctx, engine.PlaybackRequest{
		UserID: "lzy", FileSHA1: "AAAABBBB", FileName: "t.mp4", FileSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d2.Release != nil {
		d2.Release()
	}
	if d2.Branch != engine.BranchCacheHit {
		t.Fatalf("branch=%s", d2.Branch)
	}
	// me profile 统计从 DB 来
	req, _ := http.NewRequest("GET", gw.URL+"/nb/me/api/profile?nb_user=lzy", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var prof map[string]any
	decodeJSON(t, resp, &prof)
	if prof["stats"].(map[string]any)["ok"].(float64) != 2 {
		t.Fatalf("profile: %v", prof)
	}
	_ = st
}

// TestDBUserAdmin：用户改模式/模板走 SQLite（upsert），列表读 DB。
func TestDBUserAdmin(t *testing.T) {
	gw, st, _ := newDBFixture(t)
	ctx := context.Background()

	body, _ := json.Marshal(map[string]string{"mode": "115", "template": "vip"})
	req, _ := http.NewRequest("PUT", gw.URL+"/nb/admin/api/users/lzy", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	u, err := st.Users().GetUser(ctx, "lzy")
	if err != nil || u.Mode != "115" {
		t.Fatalf("user=%+v err=%v", u, err)
	}
	var users []adminUserJSON
	decodeJSON(t, dbAdminGet(t, gw.URL, "/nb/admin/api/users"), &users)
	found := false
	for _, x := range users {
		if x.ID == "lzy" && x.Mode == "115" {
			found = true
		}
	}
	if !found {
		t.Fatalf("users: %+v", users)
	}
}

// TestDBCookieFlow：个人中心存 Cookie → SQLite 加密存储 → 可解密读回。
func TestDBCookieFlow(t *testing.T) {
	gw, st, _ := newDBFixture(t)
	ctx := context.Background()

	body, _ := json.Marshal(map[string]string{"cookie": "UID=aaa;CID=bbb"})
	req, _ := http.NewRequest("POST", gw.URL+"/nb/me/api/cookie?nb_user=lzy", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cs, err := st.Cookies(testCookieKey)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := cs.GetCookie(ctx, "lzy")
	if err != nil || !ok || got != "UID=aaa;CID=bbb" {
		t.Fatalf("got=%q ok=%v err=%v", got, ok, err)
	}
	// 换密钥读不出
	cs2, _ := st.Cookies([]byte("other-key-0123456789abcdef"))
	if _, _, err := cs2.GetCookie(ctx, "lzy"); err == nil {
		t.Fatal("different key should fail")
	}
}

// TestDBPoolHealth：池摘除/恢复走 SQLite。
func TestDBPoolHealth(t *testing.T) {
	gw, st, _ := newDBFixture(t)
	ctx := context.Background()

	body, _ := json.Marshal(map[string]bool{"healthy": false})
	req, _ := http.NewRequest("POST", gw.URL+"/nb/admin/api/pool/115小1/health", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	accs, _ := st.Pool().ListAccounts(ctx)
	if accs[0].Healthy {
		t.Fatal("should be unhealthy in DB")
	}
	var pool []poolStatusJSON
	decodeJSON(t, dbAdminGet(t, gw.URL, "/nb/admin/api/pool"), &pool)
	if pool[0].Healthy {
		t.Fatalf("pool api: %+v", pool)
	}
}
