// cryptoCipher 是内部的 AES-GCM 加解密 helper（cookies.go 与 accounts115.go 共用）。
//
// 格式：nonce(12B) || ciphertext；密钥长度非 16/24/32 时做 SHA-256 规整为 32 字节。
// 高敏凭证：明文永不打日志，只存密文。
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
)

type cryptoCipher struct {
	mu  sync.Mutex
	gcm cipher.AEAD
}

func newCryptoCipher(key []byte) (*cryptoCipher, error) {
	if len(key) != 16 && len(key) != 24 && len(key) != 32 {
		sum := sha256.Sum256(key)
		key = sum[:]
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: cookie cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: cookie gcm: %w", err)
	}
	return &cryptoCipher{gcm: gcm}, nil
}

// seal 加密，返回 nonce(12B)||ciphertext。
func (c *cryptoCipher) seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: cookie nonce: %w", err)
	}
	c.mu.Lock()
	ct := c.gcm.Seal(nil, nonce, plaintext, nil)
	c.mu.Unlock()
	buf := make([]byte, 0, len(nonce)+len(ct))
	buf = append(buf, nonce...)
	buf = append(buf, ct...)
	return buf, nil
}

// open 解密 nonce(12B)||ciphertext。
func (c *cryptoCipher) open(buf []byte) ([]byte, error) {
	if len(buf) < 12 {
		return nil, fmt.Errorf("store: cookie corrupt")
	}
	c.mu.Lock()
	pt, err := c.gcm.Open(nil, buf[:12], buf[12:], nil)
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("store: cookie decrypt: %w", err)
	}
	return pt, nil
}
