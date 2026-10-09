package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 鉴权

func TestAuthFirstSetupAndLogin(t *testing.T) {
	fx := newCfgFixture(t)
	ctx := context.Background()
	loginURL := fx.gw.URL + "/nb/admin/api/login"

	// 初始：未设置密码
	var st struct {
		PasswordSet bool `json:"passwordSet"`
	}
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/login/status", nil), &st)
	if st.PasswordSet {
		t.Fatal("want passwordSet=false")
	}

	// 未设置密码时直接登录 → 401 password_not_set
	anon := newJarClient(t)
	resp := postJSON(t, anon, loginURL, map[string]string{"password": "x"})
	if resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 首次设置密码 → 200，带 session cookie
	client := newJarClient(t)
	resp = postJSON(t, client, loginURL, map[string]string{"new_password": "s3cr3t!"})
	var okBody map[string]any
	decodeJSON(t, resp, &okBody)
	if resp.StatusCode != 200 || okBody["ok"] != true {
		t.Fatalf("setup: %d %v", resp.StatusCode, okBody)
	}
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "nb_session=") || !strings.Contains(setCookie, "HttpOnly") {
		t.Fatalf("Set-Cookie = %q", setCookie)
	}

	// status → true
	decodeJSON(t, adminReq(t, fx, "GET", "/nb/admin/api/login/status", nil), &st)
	if !st.PasswordSet {
		t.Fatal("want passwordSet=true")
	}

	// session cookie 可访问受保护 API（无 token）
	req, _ := http.NewRequest("GET", fx.gw.URL+"/nb/admin/api/stats", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session auth: %d", resp.StatusCode)
	}

	// hash 存的是 v1$ 格式，不是明文
	h, hasHash, err := fx.st.GetSetting(ctx, "admin.password_hash")
	if err != nil || !hasHash {
		t.Fatalf("hash not stored: %v", err)
	}
	if !strings.HasPrefix(h, "v1$") || strings.Contains(h, "s3cr3t!") {
		t.Fatalf("bad hash format: %q", h)
	}

	// 错密码 → 401
	resp = postJSON(t, anon, loginURL, map[string]string{"password": "wrong"})
	if resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 正确密码 → 200（新 client 也能登录）
	resp = postJSON(t, anon, loginURL, map[string]string{"password": "s3cr3t!"})
	var ok2 map[string]any
	decodeJSON(t, resp, &ok2)
	if resp.StatusCode != 200 || ok2["ok"] != true {
		t.Fatalf("login: %d %v", resp.StatusCode, ok2)
	}

	// 改密码（已登录）
	resp = postJSON(t, client, loginURL, map[string]any{
		"password": "s3cr3t!", "new_password": "n3w-pw", "change": true,
	})
	var ok3 map[string]any
	decodeJSON(t, resp, &ok3)
	if resp.StatusCode != 200 || ok3["ok"] != true {
		t.Fatalf("change: %d %v", resp.StatusCode, ok3)
	}
	// 旧密码失效
	resp = postJSON(t, anon, loginURL, map[string]string{"password": "s3cr3t!"})
	if resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("old password should fail: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 新密码生效
	resp = postJSON(t, anon, loginURL, map[string]string{"password": "n3w-pw"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("new password: %d", resp.StatusCode)
	}

	// 前端设置页形态：{password: 新密码, change:true}（无 new_password 字段）
	resp = postJSON(t, client, loginURL, map[string]any{
		"password": "n3w-pw-2", "change": true,
	})
	var ok3b map[string]any
	decodeJSON(t, resp, &ok3b)
	if resp.StatusCode != 200 || ok3b["ok"] != true {
		t.Fatalf("change(frontend shape): %d %v", resp.StatusCode, ok3b)
	}
	resp = postJSON(t, anon, loginURL, map[string]string{"password": "n3w-pw-2"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("new password 2: %d", resp.StatusCode)
	}

	// 未登录改密码 → 401
	resp = postJSON(t, newJarClient(t), loginURL, map[string]any{
		"password": "n3w-pw", "new_password": "zzz", "change": true,
	})
	if resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// logout 后 session 失效
	resp = postJSON(t, client, fx.gw.URL+"/nb/admin/api/logout", nil)
	var ok4 map[string]any
	decodeJSON(t, resp, &ok4)
	if resp.StatusCode != 200 || ok4["ok"] != true {
		t.Fatalf("logout: %d %v", resp.StatusCode, ok4)
	}
	req, _ = http.NewRequest("GET", fx.gw.URL+"/nb/admin/api/stats", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("after logout: want 401, got %d", resp.StatusCode)
	}
}

func TestAuthTokenCompat(t *testing.T) {
	fx := newCfgFixture(t)
	// 旧 token 鉴权仍兼容：Bearer / ?token= 放行，无凭证 401
	req, _ := http.NewRequest("GET", fx.gw.URL+"/nb/admin/api/stats", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bearer: %d", resp.StatusCode)
	}
	resp, err = http.Get(fx.gw.URL + "/nb/admin/api/stats?token=test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("query token: %d", resp.StatusCode)
	}
	resp, err = http.Get(fx.gw.URL + "/nb/admin/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no creds: want 401, got %d", resp.StatusCode)
	}
}

func TestPasswordHashFormat(t *testing.T) {
	h, err := hashPassword("hello")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[1]) != 32 || len(parts[2]) != 64 {
		t.Fatalf("bad format: %q", h)
	}
	if !checkPassword(h, "hello") || checkPassword(h, "hell0") {
		t.Fatal("checkPassword wrong")
	}
	if checkPassword("garbage", "hello") || checkPassword("v1$zz$zz", "hello") {
		t.Fatal("malformed hash should fail")
	}
	// 两次 hash 的 salt 不同
	h2, _ := hashPassword("hello")
	if h == h2 {
		t.Fatal("salt not random")
	}
	if !checkPassword(h2, "hello") {
		t.Fatal("checkPassword h2 wrong")
	}
}
