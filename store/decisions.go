package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nextemby-replay/engine"
)

func unixTime(sec int64) time.Time { return time.Unix(sec, 0) }

// LogDecision 把一次决策快照写入 decisions 表（engine.OnDecision hook 用）。
// 决策内容不含任何凭证，可安全调用；写入失败由调用方决定如何处理
// （网关里只记日志，不影响播放）。
func (s *Store) LogDecision(sum engine.DecisionSummary) error {
	stepsJSON, err := json.Marshal(sum.Steps)
	if err != nil {
		return fmt.Errorf("store: log decision marshal steps: %w", err)
	}
	ok := 0
	if sum.Allowed {
		ok = 1
	}
	_, err = s.db.Exec(
		`INSERT INTO decisions(user_id, branch, account_id, sha1, elapsed_ms, ok, steps_json, ua, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		sum.UserID, sum.Branch, sum.AccountID, "", sum.ElapsedMs(), ok,
		string(stepsJSON), sum.UA, sum.At.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: log decision: %w", err)
	}
	return nil
}

// BranchStats 返回 sinceUnix 之后各分支的决策计数（管理后台总览用）。
func (s *Store) BranchStats(ctx context.Context, sinceUnix int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT branch, COUNT(*) FROM decisions WHERE created_at>=? GROUP BY branch`,
		sinceUnix)
	if err != nil {
		return nil, fmt.Errorf("store: branch stats: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var branch string
		var n int
		if err := rows.Scan(&branch, &n); err != nil {
			return nil, fmt.Errorf("store: branch stats scan: %w", err)
		}
		out[branch] = n
	}
	return out, rows.Err()
}

// UserStats 从 decisions 聚合每用户的直连成功/失败计数（管理后台用户列表用）。
func (s *Store) UserStats(ctx context.Context) (map[string]engine.StatPair, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, COALESCE(SUM(ok),0), COUNT(*) FROM decisions GROUP BY user_id`)
	if err != nil {
		return nil, fmt.Errorf("store: user stats: %w", err)
	}
	defer rows.Close()
	out := map[string]engine.StatPair{}
	for rows.Next() {
		var userID string
		var okSum, total int64
		if err := rows.Scan(&userID, &okSum, &total); err != nil {
			return nil, fmt.Errorf("store: user stats scan: %w", err)
		}
		out[userID] = engine.StatPair{OK: okSum, Fail: total - okSum}
	}
	return out, rows.Err()
}

// RecentDecisions 返回最近 limit 条决策（按写入倒序；管理后台播放日志用）。
func (s *Store) RecentDecisions(ctx context.Context, limit int) ([]engine.DecisionSummary, error) {
	return s.queryDecisions(ctx,
		`SELECT user_id, branch, account_id, elapsed_ms, ok, steps_json, ua, created_at
		 FROM decisions ORDER BY id DESC LIMIT ?`, limit)
}

// RecentUserDecisions 返回某用户最近 limit 条决策（个人中心用）。
func (s *Store) RecentUserDecisions(ctx context.Context, userID string, limit int) ([]engine.DecisionSummary, error) {
	return s.queryDecisions(ctx,
		`SELECT user_id, branch, account_id, elapsed_ms, ok, steps_json, ua, created_at
		 FROM decisions WHERE user_id=? ORDER BY id DESC LIMIT ?`, userID, limit)
}

func (s *Store) queryDecisions(ctx context.Context, query string, args ...any) ([]engine.DecisionSummary, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query decisions: %w", err)
	}
	defer rows.Close()
	var out []engine.DecisionSummary
	for rows.Next() {
		var d engine.DecisionSummary
		var okInt int
		var stepsJSON string
		var createdAt int64
		var elapsedMs int64
		if err := rows.Scan(&d.UserID, &d.Branch, &d.AccountID, &elapsedMs, &okInt, &stepsJSON, &d.UA, &createdAt); err != nil {
			return nil, fmt.Errorf("store: query decisions scan: %w", err)
		}
		d.Allowed = okInt != 0
		if err := json.Unmarshal([]byte(stepsJSON), &d.Steps); err != nil {
			return nil, fmt.Errorf("store: query decisions unmarshal steps: %w", err)
		}
		d.At = unixTime(createdAt)
		_ = elapsedMs // ElapsedMs() 由 Steps 重算；列保留供将来直接查询
		out = append(out, d)
	}
	return out, rows.Err()
}
