package gateway

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
)

// CookieStore 加密存储用户的 115 Cookie（个人中心填写）。
//
// 设计约束：Cookie 是高敏凭证 —— 绝不打日志、只存密文、密钥只来自
// NB_COOKIE_KEY（内存实现重启后丢失，生产待接 DB，见 README）。
type CookieStore interface {
	// SetCookie 加密保存用户 Cookie（覆盖旧值）。
	SetCookie(ctx context.Context, userID, cookie string) error
	// GetCookie 解密返回用户 Cookie；ok=false 表示未设置。
	GetCookie(ctx context.Context, userID string) (cookie string, ok bool, err error)
	// HasCookie 是否已设置（不解密）。
	HasCookie(ctx context.Context, userID string) bool
	// DeleteCookie 删除用户 Cookie。
	DeleteCookie(ctx context.Context, userID string) error
}

// memoryCookieStore 是 CookieStore 的内存 AES-GCM 实现。
type memoryCookieStore struct {
	mu   sync.Mutex
	gcm  cipher.AEAD
	data map[string][]byte // userID -> nonce(12B) || ciphertext
}

// NewMemoryCookieStore 创建内存 Cookie 存储。
// key 长度非 16/24/32 时做 SHA-256 规整为 32 字节（并在文档注明）。
func NewMemoryCookieStore(key []byte) (CookieStore, error) {
	if len(key) != 16 && len(key) != 24 && len(key) != 32 {
		sum := sha256.Sum256(key)
		key = sum[:]
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("gateway: cookie cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gateway: cookie gcm: %w", err)
	}
	return &memoryCookieStore{gcm: gcm, data: map[string][]byte{}}, nil
}

func (s *memoryCookieStore) SetCookie(_ context.Context, userID, cookie string) error {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("gateway: cookie nonce: %w", err)
	}
	ct := s.gcm.Seal(nil, nonce, []byte(cookie), nil)
	buf := make([]byte, 0, len(nonce)+len(ct))
	buf = append(buf, nonce...)
	buf = append(buf, ct...)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[userID] = buf
	return nil
}

func (s *memoryCookieStore) GetCookie(_ context.Context, userID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, ok := s.data[userID]
	if !ok || len(buf) < 12 {
		return "", false, nil
	}
	pt, err := s.gcm.Open(nil, buf[:12], buf[12:], nil)
	if err != nil {
		return "", false, fmt.Errorf("gateway: cookie decrypt: %w", err)
	}
	return string(pt), true, nil
}

func (s *memoryCookieStore) HasCookie(_ context.Context, userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[userID]
	return ok
}

func (s *memoryCookieStore) DeleteCookie(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, userID)
	return nil
}
