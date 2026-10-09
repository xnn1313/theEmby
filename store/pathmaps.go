package store

import (
	"context"
	"fmt"
)

// PathMap 是 Emby 媒体路径到 115 账号 + 子目录的映射。
type PathMap struct {
	EmbyPath  string
	AccountID string
	SubPath   string
	UpdatedAt int64
}

// ListPathMaps 枚举全部路径映射（按 Emby 路径排序）。
func (s *Store) ListPathMaps(ctx context.Context) ([]PathMap, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT emby_path, account_id, sub_path, updated_at FROM path_maps ORDER BY emby_path`)
	if err != nil {
		return nil, fmt.Errorf("store: list path maps: %w", err)
	}
	defer rows.Close()
	var out []PathMap
	for rows.Next() {
		var m PathMap
		if err := rows.Scan(&m.EmbyPath, &m.AccountID, &m.SubPath, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list path maps scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetPathMap 新增或更新一条路径映射（upsert）。
func (s *Store) SetPathMap(ctx context.Context, embyPath, accountID, subPath string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO path_maps(emby_path, account_id, sub_path, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(emby_path) DO UPDATE SET account_id=excluded.account_id,
		 sub_path=excluded.sub_path, updated_at=excluded.updated_at`,
		embyPath, accountID, subPath, nowUnix()); err != nil {
		return fmt.Errorf("store: set path map: %w", err)
	}
	return nil
}

// DeletePathMap 删除一条路径映射。
func (s *Store) DeletePathMap(ctx context.Context, embyPath string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM path_maps WHERE emby_path=?`, embyPath); err != nil {
		return fmt.Errorf("store: delete path map: %w", err)
	}
	return nil
}
