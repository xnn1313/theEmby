package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Template 是并发/额度策略模板（管理后台可配可存）。
type Template struct {
	Name             string
	MaxConcurrent    int
	MaxDevices       int
	DefaultLine      string
	DailyPlays       int
	UIDTaskLimit     int
	LockHours        int
	DailyRapid       int
	GiftDays         int
	ExpireDeleteDays int
}

const templateCols = `name, max_concurrent, max_devices, default_line, daily_plays, uid_task_limit, lock_hours, daily_rapid, gift_days, expire_delete_days`

func scanTemplateRow(row interface {
	Scan(dest ...any) error
},
) (Template, error) {
	var t Template
	if err := row.Scan(&t.Name, &t.MaxConcurrent, &t.MaxDevices, &t.DefaultLine,
		&t.DailyPlays, &t.UIDTaskLimit, &t.LockHours, &t.DailyRapid,
		&t.GiftDays, &t.ExpireDeleteDays); err != nil {
		return Template{}, err
	}
	return t, nil
}

// ListTemplates 枚举全部模板（按名称排序）。
func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+templateCols+` FROM templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list templates: %w", err)
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplateRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list templates scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTemplate 按名称取模板；不存在返回 found=false。
func (s *Store) GetTemplate(ctx context.Context, name string) (t Template, found bool, err error) {
	t, err = scanTemplateRow(s.db.QueryRowContext(ctx,
		`SELECT `+templateCols+` FROM templates WHERE name=?`, name))
	if err == sql.ErrNoRows {
		return Template{}, false, nil
	}
	if err != nil {
		return Template{}, false, fmt.Errorf("store: get template: %w", err)
	}
	return t, true, nil
}

// UpsertTemplate 新增或更新模板（全字段覆盖）。
func (s *Store) UpsertTemplate(ctx context.Context, t Template) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO templates(`+templateCols+`, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET
		   max_concurrent=excluded.max_concurrent, max_devices=excluded.max_devices,
		   default_line=excluded.default_line, daily_plays=excluded.daily_plays,
		   uid_task_limit=excluded.uid_task_limit, lock_hours=excluded.lock_hours,
		   daily_rapid=excluded.daily_rapid, gift_days=excluded.gift_days,
		   expire_delete_days=excluded.expire_delete_days, updated_at=excluded.updated_at`,
		t.Name, t.MaxConcurrent, t.MaxDevices, t.DefaultLine, t.DailyPlays,
		t.UIDTaskLimit, t.LockHours, t.DailyRapid, t.GiftDays, t.ExpireDeleteDays,
		nowUnix()); err != nil {
		return fmt.Errorf("store: upsert template: %w", err)
	}
	return nil
}

// DeleteTemplate 删除模板。
func (s *Store) DeleteTemplate(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM templates WHERE name=?`, name); err != nil {
		return fmt.Errorf("store: delete template: %w", err)
	}
	return nil
}
