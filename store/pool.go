package store

import (
	"context"
	"database/sql"
	"fmt"

	"nextemby-replay/engine"
)

// PoolStore 是 engine.PoolStore 的 SQLite 实现：分布式池 115 服务账号的
// 列表与健康状态。（24h 路由锁的记账仍在 Engine 内存，见 engine.go。）
type PoolStore struct{ db *sql.DB }

// Pool 返回池账号存储。
func (s *Store) Pool() *PoolStore { return &PoolStore{db: s.db} }

// EnsureAccount 确保账号存在（不存在则插入，幂等；已存在不覆盖健康状态与上限）。
func (p *PoolStore) EnsureAccount(ctx context.Context, accountID string, maxUsers int) error {
	if maxUsers <= 0 {
		maxUsers = 4
	}
	if _, err := p.db.ExecContext(ctx,
		`INSERT INTO pool_accounts(id, max_users, healthy, updated_at)
		 VALUES(?,?,1,?)
		 ON CONFLICT(id) DO NOTHING`,
		accountID, maxUsers, nowUnix()); err != nil {
		return fmt.Errorf("store: ensure pool account: %w", err)
	}
	return nil
}

// ListAccounts 返回全部池账号（按 ID 排序）。
func (p *PoolStore) ListAccounts(ctx context.Context) ([]engine.PoolAccount, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, max_users, healthy FROM pool_accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list pool accounts: %w", err)
	}
	defer rows.Close()
	var out []engine.PoolAccount
	for rows.Next() {
		var a engine.PoolAccount
		var healthy int
		if err := rows.Scan(&a.ID, &a.MaxUsers, &healthy); err != nil {
			return nil, fmt.Errorf("store: list pool accounts scan: %w", err)
		}
		a.Healthy = healthy != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetHealthy 设置账号健康状态；账号不存在返回 "unknown" 错误
// （管理后台据此返回 404，对齐 memory 实现语义）。
func (p *PoolStore) SetHealthy(ctx context.Context, accountID string, healthy bool) error {
	h := 0
	if healthy {
		h = 1
	}
	res, err := p.db.ExecContext(ctx,
		`UPDATE pool_accounts SET healthy=?, updated_at=? WHERE id=?`,
		h, nowUnix(), accountID)
	if err != nil {
		return fmt.Errorf("store: set pool healthy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set pool healthy: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: unknown pool account %q", accountID)
	}
	return nil
}
