// Package memory 提供 Engine 所有外部接口的 in-memory 实现，
// 让引擎不依赖任何外部服务就能跑通（测试 / 演示 / 本地联调）。
package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"nextemby-replay/engine"
)

// ---------------------------------------------------------------- UserStore

// UserStore 是 engine.UserStore 的内存实现。
type UserStore struct {
	mu    sync.Mutex
	users map[string]engine.User
}

func NewUserStore(users ...engine.User) *UserStore {
	s := &UserStore{users: map[string]engine.User{}}
	for _, u := range users {
		s.users[u.ID] = u
	}
	return s
}

func (s *UserStore) Add(u engine.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
}

func (s *UserStore) GetUser(_ context.Context, userID string) (engine.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return engine.User{}, fmt.Errorf("memory: unknown user %q", userID)
	}
	return u, nil
}

// UpdateUser 新增或更新用户记录（管理后台用户管理用）。
func (s *UserStore) UpdateUser(_ context.Context, u engine.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

// ListUsers 返回全部用户（按 ID 排序，管理后台用）。
func (s *UserStore) ListUsers(_ context.Context) ([]engine.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]engine.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------------------------------------------------------------- PoolStore

// PoolStore 是 engine.PoolStore 的内存实现。
type PoolStore struct {
	mu       sync.Mutex
	accounts []engine.PoolAccount
}

func NewPoolStore(accounts ...engine.PoolAccount) *PoolStore {
	cp := append([]engine.PoolAccount(nil), accounts...)
	return &PoolStore{accounts: cp}
}

func (s *PoolStore) ListAccounts(_ context.Context) ([]engine.PoolAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.PoolAccount(nil), s.accounts...), nil
}

func (s *PoolStore) SetHealthy(_ context.Context, accountID string, healthy bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == accountID {
			s.accounts[i].Healthy = healthy
			return nil
		}
	}
	return fmt.Errorf("memory: unknown pool account %q", accountID)
}

// --------------------------------------------------------------- DriveClient

// DriveClient 是 engine.DriveClient 的内存假实现，模拟一个 115 网盘账号。
//
// files: 已存在的文件（sha1 -> 落盘目录）。
// Fail* 开关用于注入故障；Probes/Transfers/DirectURLs 计数调用次数，
// 方便测试断言走了哪条分支。
type DriveClient struct {
	mu         sync.Mutex
	id         string
	files      map[string]string
	FailProbe  bool
	FailTransf bool
	FailURL    bool

	Probes     int
	Transfers  int
	DirectURLs int
}

func NewDriveClient(id string, existingSHA1 ...string) *DriveClient {
	d := &DriveClient{id: id, files: map[string]string{}}
	for _, s := range existingSHA1 {
		d.files[s] = "/"
	}
	return d
}

func (d *DriveClient) AccountID() string { return d.id }

func (d *DriveClient) ProbeSHA1(_ context.Context, sha1 string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.FailProbe {
		return false, errors.New("memory: probe failed (injected)")
	}
	d.Probes++
	_, ok := d.files[sha1]
	return ok, nil
}

func (d *DriveClient) RapidTransfer(_ context.Context, sha1, _ string, _ int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.FailTransf {
		return "", errors.New("memory: rapid transfer failed (injected)")
	}
	d.Transfers++
	d.files[sha1] = "/最近接收"
	return "/最近接收", nil
}

func (d *DriveClient) DirectURL(_ context.Context, sha1 string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.FailURL {
		return "", errors.New("memory: direct url failed (injected)")
	}
	d.DirectURLs++
	if _, ok := d.files[sha1]; !ok {
		return "", fmt.Errorf("memory: file %q not in drive %q", sha1, d.id)
	}
	return "https://115.example/d/" + d.id + "/" + sha1, nil
}

// HasFile 供测试断言文件是否已落盘。
func (d *DriveClient) HasFile(sha1 string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.files[sha1]
	return ok
}

// ---------------------------------------------------------------- ShieldClient

// ShieldClient 是 engine.ShieldClient 的内存假实现，模拟 NextFind 神盾接口。
type ShieldClient struct {
	mu         sync.Mutex
	index      map[string]engine.ShieldResult // sha1 -> 查询结果
	Transfers  []string                       // 已转存的 slug（按调用顺序）
	Background []string                       // 已触发的后台搜索 tmdb_id
	// OnTransfer 钩子：测试可在此把文件"种"进池盘，模拟转存生效。
	OnTransfer func(slug, targetFolder string)
}

func NewShieldClient(index map[string]engine.ShieldResult) *ShieldClient {
	cp := map[string]engine.ShieldResult{}
	for k, v := range index {
		cp[k] = v
	}
	return &ShieldClient{index: cp}
}

func (s *ShieldClient) SearchBySHA1(_ context.Context, sha1 string) (engine.ShieldResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.index[sha1]; ok {
		return r, nil
	}
	return engine.ShieldResult{Found: false}, nil
}

func (s *ShieldClient) TriggerBackgroundSearch(_ context.Context, tmdbID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Background = append(s.Background, tmdbID)
	return nil
}

func (s *ShieldClient) TransferToPool(_ context.Context, slug, targetFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transfers = append(s.Transfers, slug)
	if s.OnTransfer != nil {
		s.OnTransfer(slug, targetFolder)
	}
	return nil
}

// ------------------------------------------------------- PlaybackRecordStore

// PlaybackRecordStore 是 engine.PlaybackRecordStore 的内存实现。
// 按 sha1 记录播放过的用户（保持插入顺序，末尾最新）。
type PlaybackRecordStore struct {
	mu   sync.Mutex
	recs map[string][]string
}

func NewPlaybackRecordStore() *PlaybackRecordStore {
	return &PlaybackRecordStore{recs: map[string][]string{}}
}

func (s *PlaybackRecordStore) RecordPlayback(_ context.Context, userID, sha1 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.recs[sha1] {
		if u == userID {
			return nil // 已记录，去重
		}
	}
	s.recs[sha1] = append(s.recs[sha1], userID)
	return nil
}

func (s *PlaybackRecordStore) FindRecentPlayer(_ context.Context, sha1, excludeUserID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users := s.recs[sha1]
	for i := len(users) - 1; i >= 0; i-- {
		if users[i] != excludeUserID {
			return users[i], true, nil
		}
	}
	return "", false, nil
}
