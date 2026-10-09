package store

import (
	"context"
	"database/sql"
	"fmt"

	"nextemby-replay/engine"
)

// UserStore 是 engine.UserStore / UserUpdater / UserLister 的 SQLite 实现。
type UserStore struct{ db *sql.DB }

// Users 返回用户存储。
func (s *Store) Users() *UserStore { return &UserStore{db: s.db} }

// GetUser 按 ID 取用户；不存在返回 "unknown user" 错误（对齐 memory 实现语义）。
func (u *UserStore) GetUser(ctx context.Context, userID string) (engine.User, error) {
	var usr engine.User
	err := u.db.QueryRowContext(ctx,
		`SELECT id, mode, template FROM users WHERE id=?`, userID,
	).Scan(&usr.ID, &usr.Mode, &usr.Template)
	if err == sql.ErrNoRows {
		return engine.User{}, fmt.Errorf("store: unknown user %q", userID)
	}
	if err != nil {
		return engine.User{}, fmt.Errorf("store: get user: %w", err)
	}
	return usr, nil
}

// UpdateUser 更新用户（不存在则插入，即 upsert）。
func (u *UserStore) UpdateUser(ctx context.Context, usr engine.User) error {
	now := nowUnix()
	_, err := u.db.ExecContext(ctx,
		`INSERT INTO users(id, mode, template, created_at, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET mode=excluded.mode, template=excluded.template,
		 updated_at=excluded.updated_at`,
		usr.ID, usr.Mode, usr.Template, now, now)
	if err != nil {
		return fmt.Errorf("store: update user: %w", err)
	}
	return nil
}

// ListUsers 枚举全部用户（按 ID 排序，管理后台用）。
func (u *UserStore) ListUsers(ctx context.Context) ([]engine.User, error) {
	rows, err := u.db.QueryContext(ctx,
		`SELECT id, mode, template FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	var out []engine.User
	for rows.Next() {
		var usr engine.User
		if err := rows.Scan(&usr.ID, &usr.Mode, &usr.Template); err != nil {
			return nil, fmt.Errorf("store: list users scan: %w", err)
		}
		out = append(out, usr)
	}
	return out, rows.Err()
}
