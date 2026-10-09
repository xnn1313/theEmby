// Package store 是 nextemby-replay 的 SQLite 持久化层（pure Go，modernc.org/sqlite，
// distroless 静态构建可用）。
//
// 持久化：users / cookies（AES-GCM 加密）/ playback_records / decisions /
// pool_accounts / settings / accounts115 / path_maps / templates / syslogs。
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
	"strings"
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
CREATE TABLE IF NOT EXISTS settings(
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL DEFAULT '',
  updated_at INTEGER
);
CREATE TABLE IF NOT EXISTS accounts115(
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL DEFAULT 'pool',
  cookie_cipher BLOB,
  uid          TEXT NOT NULL DEFAULT '',
  max_users    INTEGER NOT NULL DEFAULT 4,
  healthy      INTEGER NOT NULL DEFAULT 1,
  enabled      INTEGER NOT NULL DEFAULT 1,
  rapid_dir    TEXT NOT NULL DEFAULT '/最近接收',
  quota_info   TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS path_maps(
  emby_path  TEXT PRIMARY KEY,
  account_id TEXT NOT NULL DEFAULT '',
  sub_path   TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS templates(
  name               TEXT PRIMARY KEY,
  max_concurrent     INTEGER NOT NULL DEFAULT 5,
  max_devices        INTEGER NOT NULL DEFAULT 10,
  default_line       TEXT NOT NULL DEFAULT 'pool',
  daily_plays        INTEGER NOT NULL DEFAULT -1,
  uid_task_limit     INTEGER NOT NULL DEFAULT 3,
  lock_hours         INTEGER NOT NULL DEFAULT 24,
  daily_rapid        INTEGER NOT NULL DEFAULT -1,
  gift_days          INTEGER NOT NULL DEFAULT 1,
  expire_delete_days INTEGER NOT NULL DEFAULT 7,
  updated_at         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS syslogs(
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       INTEGER NOT NULL,
  category TEXT NOT NULL DEFAULT 'system',
  message  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_syslogs_ts ON syslogs(ts DESC);
`

// Store 持有唯一的 *sql.DB（单连接池共享）。
type Store struct {
	db *sql.DB
}

// Open 打开（不存在则创建）SQLite 数据库并建表，再做老库兼容迁移。
// path 如 "./nextemby.db" 或 "/data/nextemby.db"。
//
// 并发说明：journal_mode=WAL 是库级持久设置；busy_timeout 必须每个连接都设
// （PRAGMA 是连接级），所以经 DSN 的 _pragma 参数下发——sql.DB 连接池里
// 懒建的新连接同样生效，避免并发写（决策落库 + 日志 sink）直接 SQLITE_BUSY。
func Open(path string) (*Store, error) {
	dsn := path
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// 显式再执行一次，保证老连接/老驱动行为一致（幂等）。
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
	s := &Store{db: db}
	if err := s.migrateColumns(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migratePoolAccounts(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层 *sql.DB（排障/测试用）。
func (s *Store) DB() *sql.DB { return s.db }

func nowUnix() int64 { return time.Now().Unix() }

// addColumns 是老库兼容迁移：用 PRAGMA table_info 检查缺列再 ALTER TABLE ADD COLUMN。
type addColumn struct{ name, ddl string }

func (s *Store) migrateColumns(ctx context.Context) error {
	migrations := map[string][]addColumn{
		"users": {
			{"expires_at", "INTEGER NOT NULL DEFAULT 0"},
			{"banned", "INTEGER NOT NULL DEFAULT 0"},
			{"remark", "TEXT NOT NULL DEFAULT ''"},
		},
		"decisions": {
			{"ua", "TEXT NOT NULL DEFAULT ''"},
		},
	}
	for table, cols := range migrations {
		rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			return fmt.Errorf("store: migrate table_info %s: %w", table, err)
		}
		have := map[string]bool{}
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull, pk int
			var dflt any
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("store: migrate scan %s: %w", table, err)
			}
			have[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: migrate rows %s: %w", table, err)
		}
		for _, c := range cols {
			if have[c.name] {
				continue
			}
			if _, err := s.db.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, c.name, c.ddl)); err != nil {
				return fmt.Errorf("store: migrate add %s.%s: %w", table, c.name, err)
			}
		}
	}
	return nil
}

// migratePoolAccounts 把老 pool_accounts 的行迁移进 accounts115（kind='pool'，幂等）。
func (s *Store) migratePoolAccounts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO accounts115(id, name, kind, uid, max_users, healthy, enabled, rapid_dir, quota_info, updated_at)
SELECT id, id, 'pool', '', max_users, healthy, 1, '/最近接收', '', updated_at
FROM pool_accounts`)
	if err != nil {
		return fmt.Errorf("store: migrate pool_accounts: %w", err)
	}
	return nil
}

// EnsureDefaults 写入种子数据（幂等）：用户 lzy（pool/vip）、guest（pool），
// 池账号 115小1 / 115小2（max_users=4，healthy）、种子盘 115大，
// 并发策略模板 vip，115 路径映射。已存在的数据一律不覆盖（DO NOTHING）。
// settings 不种子（管理员密码首次登录时设置）。
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
	for _, a := range []struct {
		id       string
		kind     string
		maxUsers int
	}{
		{"115大", "seed", 4},
		{"115小1", "pool", 4},
		{"115小2", "pool", 4},
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO accounts115(id, name, kind, uid, max_users, healthy, enabled, rapid_dir, quota_info, updated_at)
			 VALUES(?,?,?,?,?,1,1,'/最近接收','',?)
			 ON CONFLICT(id) DO NOTHING`,
			a.id, a.id, a.kind, "", a.maxUsers, now); err != nil {
			return fmt.Errorf("store: seed account115 %s: %w", a.id, err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO templates(name, max_concurrent, max_devices, default_line, daily_plays,
		 uid_task_limit, lock_hours, daily_rapid, gift_days, expire_delete_days, updated_at)
		 VALUES('vip', 5, 10, 'self', 3, 3, 24, -1, 1, 7, ?)
		 ON CONFLICT(name) DO NOTHING`, now); err != nil {
		return fmt.Errorf("store: seed template vip: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO path_maps(emby_path, account_id, sub_path, updated_at)
		 VALUES('/CloudNAS/CloudDrive/115open', '115大', '', ?)
		 ON CONFLICT(emby_path) DO NOTHING`, now); err != nil {
		return fmt.Errorf("store: seed path_map: %w", err)
	}
	return nil
}
