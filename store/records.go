package store

import (
	"context"
	"database/sql"
	"fmt"
)

// PlaybackRecordStore 是 engine.PlaybackRecordStore 的 SQLite 实现：
// "播放记录库"（sha1 -> 播过该文件的用户），支撑 115 模式 STEP2 用户间秒传。
type PlaybackRecordStore struct{ db *sql.DB }

// Records 返回播放记录库。
func (s *Store) Records() *PlaybackRecordStore { return &PlaybackRecordStore{db: s.db} }

// RecordPlayback 记录一次播放（(sha1,user_id) 幂等，重复记录只刷新时间）。
func (r *PlaybackRecordStore) RecordPlayback(ctx context.Context, userID, sha1 string) error {
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO playback_records(sha1, user_id, played_at) VALUES(?,?,?)
		 ON CONFLICT(sha1, user_id) DO UPDATE SET played_at=excluded.played_at`,
		sha1, userID, nowUnix()); err != nil {
		return fmt.Errorf("store: record playback: %w", err)
	}
	return nil
}

// FindRecentPlayer 返回最近播过该文件的其他用户（排除 excludeUserID）。
// 没有返回 ("", false, nil)。
func (r *PlaybackRecordStore) FindRecentPlayer(ctx context.Context, sha1, excludeUserID string) (string, bool, error) {
	var userID string
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id FROM playback_records
		 WHERE sha1=? AND user_id<>?
		 ORDER BY played_at DESC LIMIT 1`,
		sha1, excludeUserID,
	).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: find recent player: %w", err)
	}
	return userID, true, nil
}
