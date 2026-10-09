package store

import (
	"context"
	"database/sql"
	"fmt"

	"nextemby-replay/engine"
)

// rapidBranches 是"秒传类"分支：统计"今日秒传"等额度口径用。
var rapidBranches = []string{
	engine.BranchP2PRapid,         // p2p_rapid
	engine.BranchSeedFallback,     // seed_fallback
	engine.BranchPoolShieldHit,    // pool_shield_transfer
	engine.BranchPoolSeedFallback, // pool_seed_fallback
}

// directBranches 是"直连类"分支（命中即取直链，无秒传动作）。
var directBranches = []string{
	engine.BranchCacheHit,     // cache_hit
	engine.BranchOwnDriveHit,  // own_drive_hit
	engine.BranchPoolProbeHit, // pool_probe_hit
}

// CountUserDecisionsSince 返回某用户自 sinceUnix 起的决策总数。
func (s *Store) CountUserDecisionsSince(ctx context.Context, userID string, sinceUnix int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM decisions WHERE user_id=? AND created_at>=?`,
		userID, sinceUnix).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count user decisions: %w", err)
	}
	return n, nil
}

// CountUserRapidSince 返回某用户自 sinceUnix 起的秒传类分支决策数。
func (s *Store) CountUserRapidSince(ctx context.Context, userID string, sinceUnix int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM decisions
		 WHERE user_id=? AND created_at>=? AND branch IN (?,?,?,?)`,
		userID, sinceUnix,
		rapidBranches[0], rapidBranches[1], rapidBranches[2], rapidBranches[3],
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count user rapid: %w", err)
	}
	return n, nil
}

// UserActivity 返回某用户的近 30 天播放数与最近一条非空 UA
// （没有非空 UA 时 lastUA 为 ""）。
func (s *Store) UserActivity(ctx context.Context, userID string) (plays30d int, lastUA string, err error) {
	since := nowUnix() - 30*86400
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM decisions WHERE user_id=? AND created_at>=?`,
		userID, since).Scan(&plays30d); err != nil {
		return 0, "", fmt.Errorf("store: user activity count: %w", err)
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT ua FROM decisions WHERE user_id=? AND ua<>'' ORDER BY id DESC LIMIT 1`,
		userID).Scan(&lastUA)
	if err == sql.ErrNoRows {
		return plays30d, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("store: user activity ua: %w", err)
	}
	return plays30d, lastUA, nil
}

// TimelinePoint 是一个时间桶的播放计数（T=桶起始 unix 秒）。
type TimelinePoint struct {
	T     int64
	Plays int64
}

// UAPlay 是一个 UA 的播放统计；Directs 为直连类分支计数。
type UAPlay struct {
	UA      string
	Plays   int64
	Directs int64
}

// RangeAgg 是时间范围内的决策聚合（控制台总览用）。
type RangeAgg struct {
	Total    int64
	OK       int64
	ByBranch map[string]engine.StatPair
	Timeline []TimelinePoint
	ByUA     []UAPlay
}

// DecisionRangeAgg 聚合自 sinceUnix 起的决策：
// ByBranch 按 branch+ok 分组；Timeline 按 bucketSecs 分桶（<=0 取 3600）；
// ByUA 按 ua 分组（空 ua 记为 "(unknown)"）。
func (s *Store) DecisionRangeAgg(ctx context.Context, sinceUnix, bucketSecs int64) (RangeAgg, error) {
	if bucketSecs <= 0 {
		bucketSecs = 3600
	}
	out := RangeAgg{ByBranch: map[string]engine.StatPair{}}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(ok),0) FROM decisions WHERE created_at>=?`,
		sinceUnix).Scan(&out.Total, &out.OK); err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg total: %w", err)
	}
	// ByBranch：按 branch + ok 分组。
	rows, err := s.db.QueryContext(ctx,
		`SELECT branch, ok, COUNT(*) FROM decisions WHERE created_at>=? GROUP BY branch, ok`,
		sinceUnix)
	if err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg branch: %w", err)
	}
	for rows.Next() {
		var branch string
		var okInt int
		var n int64
		if err := rows.Scan(&branch, &okInt, &n); err != nil {
			rows.Close()
			return RangeAgg{}, fmt.Errorf("store: range agg branch scan: %w", err)
		}
		sp := out.ByBranch[branch]
		if okInt != 0 {
			sp.OK += n
		} else {
			sp.Fail += n
		}
		out.ByBranch[branch] = sp
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg branch rows: %w", err)
	}
	// Timeline：按 (created_at/bucketSecs) 分桶。
	trows, err := s.db.QueryContext(ctx,
		`SELECT (created_at/?)*?, COUNT(*) FROM decisions WHERE created_at>=? GROUP BY 1 ORDER BY 1`,
		bucketSecs, bucketSecs, sinceUnix)
	if err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg timeline: %w", err)
	}
	for trows.Next() {
		var p TimelinePoint
		if err := trows.Scan(&p.T, &p.Plays); err != nil {
			trows.Close()
			return RangeAgg{}, fmt.Errorf("store: range agg timeline scan: %w", err)
		}
		out.Timeline = append(out.Timeline, p)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg timeline rows: %w", err)
	}
	// ByUA：按 ua 分组；空 ua 记为 "(unknown)"。
	urows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(ua,''), '(unknown)'), COUNT(*),
		        SUM(CASE WHEN branch IN (?,?,?) THEN 1 ELSE 0 END)
		 FROM decisions WHERE created_at>=? GROUP BY 1 ORDER BY 2 DESC`,
		directBranches[0], directBranches[1], directBranches[2], sinceUnix)
	if err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg ua: %w", err)
	}
	for urows.Next() {
		var u UAPlay
		if err := urows.Scan(&u.UA, &u.Plays, &u.Directs); err != nil {
			urows.Close()
			return RangeAgg{}, fmt.Errorf("store: range agg ua scan: %w", err)
		}
		out.ByUA = append(out.ByUA, u)
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		return RangeAgg{}, fmt.Errorf("store: range agg ua rows: %w", err)
	}
	return out, nil
}
