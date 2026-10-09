package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func meCall(t *testing.T, fx *webFixture, method, path, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, fx.gw.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMeProfile(t *testing.T) {
	fx := newWebFixture(t)
	// 缺 nb_user → 400
	resp := meCall(t, fx, "GET", "/nb/me/api/profile", "")
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	// 未知用户 → 404
	resp = meCall(t, fx, "GET", "/nb/me/api/profile?nb_user=nobody", "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	// 正常
	resp = meCall(t, fx, "GET", "/nb/me/api/profile?nb_user=lzy", "")
	var p struct {
		UserID    string `json:"userID"`
		Mode      string `json:"mode"`
		HasCookie bool   `json:"hasCookie"`
	}
	func() {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
	}()
	if p.UserID != "lzy" || p.Mode != "pool" || p.HasCookie {
		t.Fatalf("profile = %+v", p)
	}
	// 页面可访问
	resp, err := http.Get(fx.gw.URL + "/nb/me/?nb_user=lzy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("me page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestMeCookieAndMode(t *testing.T) {
	fx := newWebFixture(t)
	// 空 cookie → 400
	resp := meCall(t, fx, "POST", "/nb/me/api/cookie?nb_user=lzy", `{"cookie":""}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	// 校验失败 → 400 且不存
	resp = meCall(t, fx, "POST", "/nb/me/api/cookie?nb_user=lzy", `{"cookie":"bad-cookie"}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	// 保存成功
	resp = meCall(t, fx, "POST", "/nb/me/api/cookie?nb_user=lzy", `{"cookie":"UID=good;CID=x"}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	// profile 显示已设置
	resp = meCall(t, fx, "GET", "/nb/me/api/profile?nb_user=lzy", "")
	var p struct {
		HasCookie bool `json:"hasCookie"`
	}
	func() {
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&p)
	}()
	if !p.HasCookie {
		t.Fatal("hasCookie should be true")
	}

	// 切到 115 模式（有 cookie，无 warn）
	resp = meCall(t, fx, "POST", "/nb/me/api/mode?nb_user=lzy", `{"mode":"115"}`)
	var m struct {
		Mode string `json:"mode"`
		Warn string `json:"warn"`
	}
	func() {
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&m)
	}()
	if m.Mode != "115" || m.Warn != "" {
		t.Fatalf("mode = %+v", m)
	}

	// 非法模式 → 400
	resp = meCall(t, fx, "POST", "/nb/me/api/mode?nb_user=lzy", `{"mode":"s3"}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestMeModeWithoutCookieWarns(t *testing.T) {
	fx := newWebFixture(t)
	// guest 无 cookie，切 115 → warn
	resp := meCall(t, fx, "POST", "/nb/me/api/mode?nb_user=guest", `{"mode":"115"}`)
	var m struct {
		Warn string `json:"warn"`
	}
	func() {
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&m)
	}()
	if m.Warn != "no_cookie_bound" {
		t.Fatalf("mode = %+v", m)
	}
}

// TestMeCookieEndToEnd115：存 Cookie + 切 115 模式后，播放走用户自有盘
// （STEP3 兜底秒传进用户盘，直链被改写）。
func TestMeCookieEndToEnd115(t *testing.T) {
	fx := newWebFixture(t)
	resp := meCall(t, fx, "POST", "/nb/me/api/cookie?nb_user=lzy", `{"cookie":"UID=good"}`)
	resp.Body.Close()
	resp = meCall(t, fx, "POST", "/nb/me/api/mode?nb_user=lzy", `{"mode":"115"}`)
	resp.Body.Close()

	resp = postPlayback(t, fx, "lzy")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("playback: %d", resp.StatusCode)
	}
	var doc struct {
		MediaSources []struct {
			DirectStreamUrl string `json:"DirectStreamUrl"`
		} `json:"MediaSources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.MediaSources) == 0 || !strings.HasPrefix(doc.MediaSources[0].DirectStreamUrl, "/nb/stream?t=") {
		t.Fatalf("not hijacked: %+v", doc)
	}
	// 引擎决策应走 115 模式 STEP3（自有盘空 → 记录库空 → 源盘兜底）
	decs := fx.eng.RecentDecisions(1)
	if len(decs) != 1 || decs[0].Branch != "seed_fallback" || decs[0].AccountID != "user:lzy" {
		t.Fatalf("decisions = %+v", decs)
	}
}

// TestNoSecretsInLogs：管理操作 + Cookie 保存后，日志里不得出现
// admin token / Cookie 明文 / 上游 API Key。
func TestNoSecretsInLogs(t *testing.T) {
	fx := newWebFixture(t)
	cookie := "UID=topsecret-cookie-zzz;CID=1"
	resp := meCall(t, fx, "POST", "/nb/me/api/cookie?nb_user=lzy", `{"cookie":"`+cookie+`"}`)
	resp.Body.Close()
	r := adminGet(t, fx, "/nb/admin/api/stats", "test-admin-token")
	r.Body.Close()
	logs := fx.logBuf.String()
	for _, secret := range []string{"test-admin-token", cookie, testAPIKey} {
		if strings.Contains(logs, secret) {
			t.Fatalf("secret leaked in logs: %.20s...", secret)
		}
	}
}
