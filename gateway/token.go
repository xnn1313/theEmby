package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// streamTokenExpiry 是 /nb/stream token 有效期（对标 SPEC：2h）。
const streamTokenExpiry = 2 * time.Hour

// streamClaims 是 token 载荷：只放定位一次直链所需的最少字段，
// 不放任何密钥或用户隐私信息。
type streamClaims struct {
	Exp  int64  `json:"exp"`  // 过期时间 unix 秒
	Acct string `json:"acct"` // 服务账号，如 "115小2"
	SHA1 string `json:"sha1"` // 文件 SHA1 hex
}

// issueStreamToken 签发 token，格式：base64url(json).hex(hmac-sha256)。
func issueStreamToken(key []byte, accountID, sha1 string, now time.Time) string {
	c := streamClaims{Exp: now.Add(streamTokenExpiry).Unix(), Acct: accountID, SHA1: sha1}
	raw, _ := json.Marshal(c) // 固定结构，marshal 不会失败
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyStreamToken 验签并校验有效期，失败返回 error。
func verifyStreamToken(key []byte, token string, now time.Time) (streamClaims, error) {
	var c streamClaims
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return c, fmt.Errorf("gateway: token 格式错误")
	}
	payload, sigHex := parts[0], parts[1]
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sigHex), []byte(want)) {
		return c, fmt.Errorf("gateway: token 签名无效")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return c, fmt.Errorf("gateway: token 解码失败: %w", err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("gateway: token 解析失败: %w", err)
	}
	if now.Unix() > c.Exp {
		return c, fmt.Errorf("gateway: token 已过期")
	}
	if c.Acct == "" || c.SHA1 == "" {
		return c, fmt.Errorf("gateway: token 缺少字段")
	}
	return c, nil
}
