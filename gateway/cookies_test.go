package gateway

import (
	"context"
	"strings"
	"testing"
)

func TestCookieEncryptRoundtrip(t *testing.T) {
	ctx := context.Background()
	secret := "UID=aaa;CID=bbb;SEID=ccc"

	cs, err := NewMemoryCookieStore([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SetCookie(ctx, "lzy", secret); err != nil {
		t.Fatal(err)
	}
	if !cs.HasCookie(ctx, "lzy") {
		t.Fatal("HasCookie = false")
	}
	got, ok, err := cs.GetCookie(ctx, "lzy")
	if err != nil || !ok || got != secret {
		t.Fatalf("got %q %v %v", got, ok, err)
	}
	// 未设置的用户
	if _, ok, _ := cs.GetCookie(ctx, "nobody"); ok {
		t.Fatal("unexpected cookie")
	}
	// 密文与明文不同（同包可访问内部）
	stored := cs.(*memoryCookieStore).data["lzy"]
	if string(stored) == secret || strings.Contains(string(stored), "UID=aaa") {
		t.Fatal("cookie stored in plaintext")
	}
	// 删除
	if err := cs.DeleteCookie(ctx, "lzy"); err != nil {
		t.Fatal(err)
	}
	if cs.HasCookie(ctx, "lzy") {
		t.Fatal("delete failed")
	}
	// 换密钥解不开：把旧密文塞进新密钥的存储，解密必须失败
	//（模拟 NB_COOKIE_KEY 变更后旧 Cookie 不可读）。
	storeB, _ := NewMemoryCookieStore([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	storeB.(*memoryCookieStore).data["lzy"] = stored
	if _, _, err := storeB.GetCookie(ctx, "lzy"); err == nil {
		t.Fatal("cross-key decrypt should fail")
	}
	// 非法长度密钥 → SHA-256 规整后可用
	short, err := NewMemoryCookieStore([]byte("short"))
	if err != nil {
		t.Fatal(err)
	}
	if err := short.SetCookie(ctx, "x", secret); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := short.GetCookie(ctx, "x"); !ok || got != secret {
		t.Fatal("short key roundtrip failed")
	}
}
