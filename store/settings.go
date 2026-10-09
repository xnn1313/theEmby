package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GetSetting 按 key 取配置；不存在返回 ok=false。
func (s *Store) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get setting: %w", err)
	}
	return value, true, nil
}

// SetSetting 写入/覆盖一条配置（upsert）。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value,
		 updated_at=excluded.updated_at`,
		key, value, nowUnix()); err != nil {
		return fmt.Errorf("store: set setting: %w", err)
	}
	return nil
}

// SettingsByPrefix 返回所有 key 以 prefix 开头的配置（prefix 为 "" 时返回全部）。
func (s *Store) SettingsByPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	// 转义 LIKE 通配符，保证按字面前缀匹配。
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key LIKE ? ESCAPE '\' ORDER BY key`,
		esc+"%")
	if err != nil {
		return nil, fmt.Errorf("store: settings by prefix: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: settings by prefix scan: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
