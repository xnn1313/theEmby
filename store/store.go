// Package store 是 nextemby-replay 的 SQLite 持久化层（pure Go，modernc.org/sqlite，
// distroless 静态构建可用）。
//
// 持久化：users / cookies（AES-GCM 加密）/ playback_records / decisions / pool_accounts。
//
// Transient（不入库，重启丢失只触发一次重新探测，不影响正确性）：
//   - 10min 直链缓存（engine 内存）
//   - 24h 路由锁（engine 内存；锁过期后文件副本进入可 GC）
//   - 并发会话计数（engine 内存）
//
// 本包只 import engine，不 import gateway；CookieStore 等接口按结构化签名实现，
// gateway 侧直接当 gateway.CookieStore 用。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users(
  id         TEXT PRIMARY KEY,
  mode       TEXT NOT NULL DEFAULT 'pool',
  template   TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS cookies(
  user_id    TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS playback_records(
  sha1      TEXT NOT NULL,
  user_id   TEXT NOT NULL,
  played_at INTEGER NOT NULL,
  PRIMARY KEY(sha1, user_id)
);
CREATE INDEX IF NOT EXISTS idx_playback_records_sha1 ON playback_records(sha1, played_at DESC);
CREATE TABLE IF NOT EXISTS decisions(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    TEXT NOT NULL DEFAULT '',
  branch     TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL DEFAULT '',
  sha1       TEXT NOT NULL DEFAULT '',
  elapsed_ms INTEGER NOT NULL DEFAULT 0,
  ok         INTEGER NOT NULL DEFAULT 1,
  steps_json TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_decisions_created ON decisions(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_decisions_user_created ON decisions(user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS pool_accounts(
  id         TEXT PRIMARY KEY,
  max_users  INTEGER NOT NULL DEFAULT 4,
  healthy    INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
);
`

// Store 持有唯一的 *sql.DB（单连接池共享）。
type Store struct {
	db *sql.DB
}

// Open 打开（不存在则创建）SQLite 数据库并建表。
// path 如 "./nextemby.db" 或 "/data/nextemby.db"。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// 单写者 + 并发读：WAL + busy_timeout。
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: journal_mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: busy_timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层 *sql.DB（排障/测试用）。
func (s *Store) DB() *sql.DB { return s.db }

func nowUnix() int64 { return time.Now().Unix() }

// EnsureDefaults 写入演示种子数据（幂等）：用户 lzy（pool/vip）、guest（pool），
// 池账号 115小1 / 115小2（max_users=4，healthy）。
func (s *Store) EnsureDefaults(ctx context.Context) error {
	now := nowUnix()
	for _, u := range []struct{ id, mode, template string }{
		{"lzy", "pool", "vip"},
		{"guest", "pool", ""},
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO users(id, mode, template, created_at, updated_at)
			 VALUES(?,?,?,?,?)
			 ON CONFLICT(id) DO NOTHING`,
			u.id, u.mode, u.template, now, now); err != nil {
			return fmt.Errorf("store: seed user %s: %w", u.id, err)
		}
	}
	for _, a := range []string{"115小1", "115小2"} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO pool_accounts(id, max_users, healthy, updated_at)
			 VALUES(?,?,1,?)
			 ON CONFLICT(id) DO NOTHING`,
			a, 4, now); err != nil {
			return fmt.Errorf("store: seed pool account %s: %w", a, err)
		}
	}
	return nil
}
