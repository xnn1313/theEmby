package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Account115 是 115 网盘账号的一条管理记录（seed=源盘，pool=分布式池服务账号）。
// HasCookie 只表示是否已绑定；cookie 明文永不回显、永不打日志。
type Account115 struct {
	ID        string
	Name      string
	Kind      string // seed | pool
	UID       string
	HasCookie bool
	MaxUsers  int
	Healthy   bool
	Enabled   bool
	RapidDir  string
	QuotaInfo string
	UpdatedAt int64
}

// Account115Input 是 UpsertAccount115 的入参。
// Cookie 非空时加密保存；为空时保留旧 cookie（不覆盖）。
type Account115Input struct {
	ID       string
	Name     string
	Kind     string
	Cookie   string
	UID      string
	MaxUsers int
	Healthy  bool
	Enabled  bool
	RapidDir string
}

// Accounts115Store 是 accounts115 表的存储：账号档案 + cookie 密文。
// Cookie 高敏：解密只供网关内部建 115 client；列表与 Get 永不返回明文。
type Accounts115Store struct {
	cc *cryptoCipher
	db *sql.DB
}

// Accounts115 返回 115 账号存储。key 与 Cookies 同源（NB_COOKIE_KEY）。
func (s *Store) Accounts115(key []byte) (*Accounts115Store, error) {
	cc, err := newCryptoCipher(key)
	if err != nil {
		return nil, err
	}
	return &Accounts115Store{cc: cc, db: s.db}, nil
}

func scanAccount115(rows *sql.Rows) (Account115, error) {
	var a Account115
	var healthy, enabled int
	var cipherBuf []byte
	var uid, quotaInfo sql.NullString
	if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &cipherBuf, &uid, &a.MaxUsers,
		&healthy, &enabled, &a.RapidDir, &quotaInfo, &a.UpdatedAt); err != nil {
		return Account115{}, err
	}
	a.UID = uid.String
	a.HasCookie = len(cipherBuf) > 0
	a.Healthy = healthy != 0
	a.Enabled = enabled != 0
	a.QuotaInfo = quotaInfo.String
	return a, nil
}

const account115Cols = `id, name, kind, cookie_cipher, uid, max_users, healthy, enabled, rapid_dir, quota_info, updated_at`

// ListAccounts115 枚举全部 115 账号（按 kind, name 排序）。
func (a *Accounts115Store) ListAccounts115(ctx context.Context) ([]Account115, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+account115Cols+` FROM accounts115 ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts115: %w", err)
	}
	defer rows.Close()
	var out []Account115
	for rows.Next() {
		acc, err := scanAccount115(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list accounts115 scan: %w", err)
		}
		out = append(out, acc)
	}
	return out, rows.Err()
}

// GetAccount115 按 ID 取账号；不存在返回含 "unknown" 的错误。
func (a *Accounts115Store) GetAccount115(ctx context.Context, id string) (Account115, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+account115Cols+` FROM accounts115 WHERE id=?`, id)
	if err != nil {
		return Account115{}, fmt.Errorf("store: get account115: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Account115{}, fmt.Errorf("store: unknown 115 account %q", id)
	}
	acc, err := scanAccount115(rows)
	if err != nil {
		return Account115{}, fmt.Errorf("store: get account115 scan: %w", err)
	}
	return acc, rows.Err()
}

// UpsertAccount115 新增或更新账号档案。Cookie 非空则加密保存，
// 为空则保留旧 cookie（不覆盖）。已存在时 enabled/quota_info 保持原值。
func (a *Accounts115Store) UpsertAccount115(ctx context.Context, in Account115Input) error {
	kind := in.Kind
	if kind != "seed" && kind != "pool" {
		kind = "pool"
	}
	maxUsers := in.MaxUsers
	if maxUsers <= 0 {
		maxUsers = 4
	}
	rapidDir := in.RapidDir
	if rapidDir == "" {
		rapidDir = "/最近接收"
	}
	healthy, enabled := 0, 0
	if in.Healthy {
		healthy = 1
	}
	if in.Enabled {
		enabled = 1
	}
	var cipherBuf []byte
	if in.Cookie != "" {
		buf, err := a.cc.seal([]byte(in.Cookie))
		if err != nil {
			return err
		}
		cipherBuf = buf
	} else {
		// 保留旧 cookie（无旧行时为 NULL）。
		if err := a.db.QueryRowContext(ctx,
			`SELECT cookie_cipher FROM accounts115 WHERE id=?`, in.ID,
		).Scan(&cipherBuf); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("store: upsert account115 keep cookie: %w", err)
		}
	}
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO accounts115(id, name, kind, cookie_cipher, uid, max_users, healthy, enabled, rapid_dir, quota_info, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,'',?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name, kind=excluded.kind, cookie_cipher=excluded.cookie_cipher,
		   uid=excluded.uid, max_users=excluded.max_users, healthy=excluded.healthy,
		   enabled=excluded.enabled, rapid_dir=excluded.rapid_dir, updated_at=excluded.updated_at`,
		in.ID, in.Name, kind, cipherBuf, in.UID, maxUsers, healthy, enabled, rapidDir, nowUnix())
	if err != nil {
		return fmt.Errorf("store: upsert account115: %w", err)
	}
	return nil
}

// DeleteAccount115 删除账号档案（含 cookie 密文）。
func (a *Accounts115Store) DeleteAccount115(ctx context.Context, id string) error {
	if _, err := a.db.ExecContext(ctx,
		`DELETE FROM accounts115 WHERE id=?`, id); err != nil {
		return fmt.Errorf("store: delete account115: %w", err)
	}
	return nil
}

// AccountCookie 解密返回账号 cookie，供网关内部建 115 client。
// 账号不存在或未绑定 cookie 返回 ok=false。
func (a *Accounts115Store) AccountCookie(ctx context.Context, id string) (cookie string, ok bool, err error) {
	var buf []byte
	err = a.db.QueryRowContext(ctx,
		`SELECT cookie_cipher FROM accounts115 WHERE id=?`, id,
	).Scan(&buf)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: account cookie: %w", err)
	}
	if len(buf) == 0 {
		return "", false, nil
	}
	pt, err := a.cc.open(buf)
	if err != nil {
		return "", false, err
	}
	return string(pt), true, nil
}

// CookieMask 返回展示用的 cookie 状态掩码：明文永不回显。
func CookieMask(a Account115) string {
	if !a.HasCookie {
		return "未绑定"
	}
	if a.UID != "" {
		return "UID:" + a.UID + " | ••••••••"
	}
	return "已绑定 | ••••••••"
}
