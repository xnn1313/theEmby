package store

import (
	"context"
	"database/sql"
	"fmt"
)

// CookieStore 是 115 Cookie 的 SQLite AES-GCM 加密实现（key 来自 NB_COOKIE_KEY）。
//
// 格式与 gateway.memoryCookieStore 完全一致：nonce(12B) || ciphertext；
// 方法签名与 gateway.CookieStore 结构一致，可直接赋值使用。
// Cookie 是高敏凭证：绝不打日志，只存密文。
type CookieStore struct {
	cc *cryptoCipher
	db *sql.DB
}

// Cookies 返回 Cookie 存储。key 来自 NB_COOKIE_KEY（与 gateway 配置同一来源）。
func (s *Store) Cookies(key []byte) (*CookieStore, error) {
	cc, err := newCryptoCipher(key)
	if err != nil {
		return nil, err
	}
	return &CookieStore{cc: cc, db: s.db}, nil
}

// SetCookie 加密保存用户 Cookie（覆盖旧值）。
func (c *CookieStore) SetCookie(ctx context.Context, userID, cookie string) error {
	buf, err := c.cc.seal([]byte(cookie))
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO cookies(user_id, ciphertext, updated_at) VALUES(?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET ciphertext=excluded.ciphertext,
		 updated_at=excluded.updated_at`,
		userID, buf, nowUnix()); err != nil {
		return fmt.Errorf("store: set cookie: %w", err)
	}
	return nil
}

// GetCookie 解密返回用户 Cookie；ok=false 表示未设置。
func (c *CookieStore) GetCookie(ctx context.Context, userID string) (string, bool, error) {
	var buf []byte
	err := c.db.QueryRowContext(ctx,
		`SELECT ciphertext FROM cookies WHERE user_id=?`, userID,
	).Scan(&buf)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get cookie: %w", err)
	}
	pt, err := c.cc.open(buf)
	if err != nil {
		return "", false, err
	}
	return string(pt), true, nil
}

// HasCookie 是否已设置（不解密）。
func (c *CookieStore) HasCookie(ctx context.Context, userID string) bool {
	var one int
	err := c.db.QueryRowContext(ctx,
		`SELECT 1 FROM cookies WHERE user_id=?`, userID,
	).Scan(&one)
	return err == nil
}

// DeleteCookie 删除用户 Cookie。
func (c *CookieStore) DeleteCookie(ctx context.Context, userID string) error {
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM cookies WHERE user_id=?`, userID); err != nil {
		return fmt.Errorf("store: delete cookie: %w", err)
	}
	return nil
}
