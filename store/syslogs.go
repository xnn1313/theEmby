package store

import (
	"context"
	"fmt"
)

// syslogKeep 是 syslogs 表保留的最大条数；WriteLog 后 trim 到该值。
const syslogKeep = 50000

// SysLog 是一条系统日志。
type SysLog struct {
	ID       int64
	Ts       int64
	Category string
	Message  string
}

// WriteLog 写入一条系统日志，随后把表 trim 到最近 syslogKeep 条。
func (s *Store) WriteLog(ctx context.Context, category, message string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO syslogs(ts, category, message) VALUES(?,?,?)`,
		nowUnix(), category, message); err != nil {
		return fmt.Errorf("store: write log: %w", err)
	}
	return s.trimSysLogs(ctx, syslogKeep)
}

// trimSysLogs 只保留最近 keep 条（按 id 倒序）；表行数 ≤ keep 时直接跳过；
// keep<=0 时清空。
func (s *Store) trimSysLogs(ctx context.Context, keep int) error {
	if keep <= 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM syslogs`)
		if err != nil {
			return fmt.Errorf("store: trim logs: %w", err)
		}
		return nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM syslogs`).Scan(&n); err != nil {
		return fmt.Errorf("store: trim logs count: %w", err)
	}
	if n <= keep {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
DELETE FROM syslogs
WHERE id < (SELECT MIN(id) FROM (SELECT id FROM syslogs ORDER BY id DESC LIMIT ?))`, keep)
	if err != nil {
		return fmt.Errorf("store: trim logs: %w", err)
	}
	return nil
}

// ListLogs 返回最近 limit 条日志（category 为 "" 时不过滤，按 ts DESC；
// limit<=0 取默认 100，上限 1000）。
func (s *Store) ListLogs(ctx context.Context, category string, limit int) ([]SysLog, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, ts, category, message FROM syslogs`
	var args []any
	if category != "" {
		query += ` WHERE category=?`
		args = append(args, category)
	}
	query += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list logs: %w", err)
	}
	defer rows.Close()
	var out []SysLog
	for rows.Next() {
		var l SysLog
		if err := rows.Scan(&l.ID, &l.Ts, &l.Category, &l.Message); err != nil {
			return nil, fmt.Errorf("store: list logs scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClearLogs 清空系统日志。
func (s *Store) ClearLogs(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM syslogs`); err != nil {
		return fmt.Errorf("store: clear logs: %w", err)
	}
	return nil
}
