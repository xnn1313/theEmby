package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrPoolExhausted 表示没有可用的池账号（全满 / 全不健康）。
	ErrPoolExhausted = errors.New("engine: no pool account available")
)

// Config 调整引擎行为；零值由 withDefaults 补齐。
type Config struct {
	// CacheTTL 直链缓存 TTL（SPEC：10 分钟）。
	CacheTTL time.Duration
	// LockTTL 24h 路由锁 TTL（SPEC：24 小时）。
	LockTTL time.Duration
	// Templates 并发模板名 -> 同播上限，如 {"vip": 5}。
	Templates map[string]int
	// TemplateOf 按名解析并发策略模板（如读 DB templates 表）；非 nil 时优先于
	// Templates 使用。返回 (Template, false) 表示未命中，回退 DefaultLimit。
	// MaxConcurrent<=0 的模板视为未配置，同样回退 DefaultLimit。
	TemplateOf func(name string) (Template, bool)
	// DefaultLimit 模板未命中时的默认同播上限。
	DefaultLimit int
	// ReceiveDir 秒传落盘目录（SPEC："/最近接收"）。
	ReceiveDir string
	// SeedAccountID 种子账号标识（SPEC："115大"），仅用于决策追踪；
	// 115 秒传基于哈希全局去重，不需要种子账号在线参与。
	SeedAccountID string
	// Now 时钟注入（测试用），默认 time.Now。
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.CacheTTL <= 0 {
		c.CacheTTL = 10 * time.Minute
	}
	if c.LockTTL <= 0 {
		c.LockTTL = 24 * time.Hour
	}
	if c.DefaultLimit <= 0 {
		c.DefaultLimit = 1
	}
	if c.ReceiveDir == "" {
		c.ReceiveDir = "/最近接收"
	}
	if c.SeedAccountID == "" {
		c.SeedAccountID = "115大"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Templates == nil {
		c.Templates = map[string]int{}
	}
	return c
}

// Deps 是引擎的外部依赖，全部走 interface（见 interfaces.go）。
type Deps struct {
	Users   UserStore
	Pool    PoolStore
	Seed    DriveClient            // 种子账号 client（115大），主要用于追踪与将来扩展
	Own     map[string]DriveClient // userID -> 用户自有 115（115 模式）
	PoolDrv map[string]DriveClient // accountID -> 池账号 client（池模式）
	Shield  ShieldClient
	Records PlaybackRecordStore
}

type cacheEntry struct {
	url       string
	expiresAt time.Time
}

type lockEntry struct {
	accountID string
	expiresAt time.Time
}

type userStats struct {
	success int64
	fail    int64
}

// Engine 是播放决策引擎：纯逻辑，零外部依赖（依赖全部经 Deps 注入）。
type Engine struct {
	cfg  Config
	deps Deps

	mu       sync.Mutex
	sessions map[string]int        // userID -> 活跃播放会话数
	cache    map[string]cacheEntry // accountID|sha1 -> 直链
	locks    map[string]lockEntry  // userID -> 池账号路由锁（24h）
	stats    map[string]*userStats // userID -> 直连成功/失败计数

	ownMu sync.RWMutex // 保护 deps.Own 的动态读写（个人中心绑定用户 115 后调用 SetOwnDrive）

	// OnDecision 是可选的决策持久化 hook：在每次 Handle 结束（无论允许/拒绝/异常）
	// 时调用，nil 安全。典型实现把 DecisionSummary 写入 SQLite decisions 表。
	// 注意：调用是同步的，实现里不要做慢操作；不要回调 Engine 方法（避免死锁）。
	OnDecision func(DecisionSummary)

	// LogFunc 是可选的中文播放日志 hook：播放决策关键节点调用，
	// category 取值 play | user | error | system，nil 时静默。
	// 日志文案里绝不输出 Cookie / API Key 等敏感信息。
	// 注意：调用是同步的，实现里不要做慢操作。
	LogFunc func(category, message string)

	decMu     sync.Mutex
	decisions []DecisionSummary // 决策 ring buffer（最近 200 条，供管理后台）
}

// New 创建引擎实例。
func New(cfg Config, deps Deps) *Engine {
	return &Engine{
		cfg:      cfg.withDefaults(),
		deps:     deps,
		sessions: map[string]int{},
		cache:    map[string]cacheEntry{},
		locks:    map[string]lockEntry{},
		stats:    map[string]*userStats{},
	}
}

func cacheKey(accountID, sha1 string) string { return accountID + "|" + sha1 }

// PoolAccounts 返回池账号列表（管理后台用）。
func (e *Engine) PoolAccounts(ctx context.Context) ([]PoolAccount, error) {
	return e.deps.Pool.ListAccounts(ctx)
}

// Users 返回用户存储接口（管理后台对 UserLister / UserUpdater 做类型断言用）。
func (e *Engine) Users() UserStore {
	return e.deps.Users
}

// SetPoolAccountHealthy 供将来的 API 监控接入：标记池账号健康状态。
func (e *Engine) SetPoolAccountHealthy(ctx context.Context, accountID string, healthy bool) error {
	return e.deps.Pool.SetHealthy(ctx, accountID, healthy)
}

// maxDecisions 是决策 ring buffer 上限。
const maxDecisions = 200

// SetOwnDrive 绑定/更新用户自有 115 网盘 client。
// 个人中心存 Cookie 后网关调用此方法，后续 115 模式 STEP1 即走该 client。
func (e *Engine) SetOwnDrive(userID string, drv DriveClient) {
	e.ownMu.Lock()
	defer e.ownMu.Unlock()
	if e.deps.Own == nil {
		e.deps.Own = map[string]DriveClient{}
	}
	e.deps.Own[userID] = drv
}

func (e *Engine) ownDrive(userID string) DriveClient {
	e.ownMu.RLock()
	defer e.ownMu.RUnlock()
	return e.deps.Own[userID]
}

// recordDecision 把一次决策记入 ring buffer（只保留最近 200 条），并返回快照。
func (e *Engine) recordDecision(userID string, d Decision) DecisionSummary {
	steps := make([]StepTrace, len(d.Steps))
	copy(steps, d.Steps)
	s := DecisionSummary{
		At:         e.cfg.Now(),
		UserID:     userID,
		Branch:     d.Branch,
		Allowed:    d.Allowed,
		DenyReason: d.DenyReason,
		AccountID:  d.AccountID,
		UA:         d.UA,
		Steps:      steps,
	}
	e.decMu.Lock()
	defer e.decMu.Unlock()
	if s.Branch == "" {
		s.Branch = "error"
	}
	e.decisions = append(e.decisions, s)
	if len(e.decisions) > maxDecisions {
		e.decisions = append([]DecisionSummary(nil), e.decisions[len(e.decisions)-maxDecisions:]...)
	}
	return s
}

// RecentDecisions 返回最近 n 条决策（按时间倒序，n<=0 返回全部）。
func (e *Engine) RecentDecisions(n int) []DecisionSummary {
	e.decMu.Lock()
	defer e.decMu.Unlock()
	out := make([]DecisionSummary, len(e.decisions))
	copy(out, e.decisions)
	// 倒序：最新的在前。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// AllUserStats 返回全部用户的直连成功/失败计数（管理后台总览用）。
func (e *Engine) AllUserStats() map[string]StatPair {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]StatPair, len(e.stats))
	for id, s := range e.stats {
		out[id] = StatPair{OK: s.success, Fail: s.fail}
	}
	return out
}

// PoolLocks 返回未过期的 24h 路由锁（管理后台池占用用）。
func (e *Engine) PoolLocks() []PoolLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.Now()
	var out []PoolLock
	for userID, l := range e.locks {
		if now.Before(l.expiresAt) {
			out = append(out, PoolLock{UserID: userID, AccountID: l.accountID, ExpiresAt: l.expiresAt})
		}
	}
	return out
}

// Stats 返回用户的直连成功/失败计数。
func (e *Engine) Stats(userID string) (success, fail int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.stats[userID]; ok {
		return s.success, s.fail
	}
	return 0, 0
}

// Sessions 返回用户当前的活跃播放会话数（主要用于测试/观测）。
func (e *Engine) Sessions(userID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[userID]
}

// resolveLimit 解析用户的同播上限：TemplateOf 非 nil 时优先使用它
// （MaxConcurrent>0 才生效），否则走旧的 Templates map；
// 解析失败或上限 <=0 时回退 DefaultLimit（保持旧语义）。
func (e *Engine) resolveLimit(user User) int {
	if e.cfg.TemplateOf != nil {
		if t, ok := e.cfg.TemplateOf(user.Template); ok && t.MaxConcurrent > 0 {
			return t.MaxConcurrent
		}
	} else if l := e.cfg.Templates[user.Template]; l > 0 {
		return l
	}
	return e.cfg.DefaultLimit
}

// acquire 按模板做并发准入；成功返回释放函数。
func (e *Engine) acquire(user User) (func(), bool) {
	limit := e.resolveLimit(user)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessions[user.ID] >= limit {
		return nil, false
	}
	e.sessions[user.ID]++
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.sessions[user.ID] > 0 {
			e.sessions[user.ID]--
		}
	}, true
}

func (e *Engine) getCache(accountID, sha1 string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := cacheKey(accountID, sha1)
	ce, ok := e.cache[k]
	if !ok || !e.cfg.Now().Before(ce.expiresAt) {
		delete(e.cache, k) // 懒惰过期
		return "", false
	}
	return ce.url, true
}

func (e *Engine) setCache(accountID, sha1, url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cache[cacheKey(accountID, sha1)] = cacheEntry{url: url, expiresAt: e.cfg.Now().Add(e.cfg.CacheTTL)}
}

func (e *Engine) bumpStat(userID string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, found := e.stats[userID]
	if !found {
		s = &userStats{}
		e.stats[userID] = s
	}
	if ok {
		s.success++
	} else {
		s.fail++
	}
}

// renewLock 在秒传/转存成功后建立或续期 24h 路由锁。
func (e *Engine) renewLock(userID, accountID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.locks[userID] = lockEntry{accountID: accountID, expiresAt: e.cfg.Now().Add(e.cfg.LockTTL)}
}

// poolRoute 解析池模式用户的服务账号：有未过期的 24h 锁直接用，
// 否则分配到当前锁定用户数最少的健康账号（SPEC §9）。
// 返回 (accountID, routeBranch, err)，routeBranch 为 "locked" / "assigned"。
func (e *Engine) poolRoute(ctx context.Context, userID string) (string, string, error) {
	e.mu.Lock()
	if l, ok := e.locks[userID]; ok && e.cfg.Now().Before(l.expiresAt) {
		e.mu.Unlock()
		return l.accountID, "locked", nil
	}
	e.mu.Unlock()

	accounts, err := e.deps.Pool.ListAccounts(ctx)
	if err != nil {
		return "", "", fmt.Errorf("engine: list pool accounts: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.Now()
	// 双重检查：ListAccounts 期间可能已有其他 goroutine 建锁。
	if l, ok := e.locks[userID]; ok && now.Before(l.expiresAt) {
		return l.accountID, "locked", nil
	}
	counts := map[string]int{}
	for _, l := range e.locks {
		if now.Before(l.expiresAt) {
			counts[l.accountID]++
		}
	}
	var best *PoolAccount
	for i := range accounts {
		a := &accounts[i]
		if !a.Healthy {
			continue
		}
		if counts[a.ID] >= a.MaxUsersOrDefault() {
			continue
		}
		if best == nil || counts[a.ID] < counts[best.ID] {
			best = a
		}
	}
	if best == nil {
		return "", "", ErrPoolExhausted
	}
	e.locks[userID] = lockEntry{accountID: best.ID, expiresAt: now.Add(e.cfg.LockTTL)}
	return best.ID, "assigned", nil
}

// Handle 是完整入口：并发准入 + 播放决策链。
//
// 拒绝（超限/池满等）是正常决策：返回 Allowed=false 的 Decision 而不是 error。
// I/O 失败返回 error。注意：只要 Decision.Release 非 nil，调用方必须在播放结束
// （或出错放弃）时调用它释放会话，否则会话计数会泄漏。
func (e *Engine) Handle(ctx context.Context, req PlaybackRequest) (d Decision, err error) {
	now := e.cfg.Now
	// 无论允许/拒绝/异常，都记入 ring buffer（管理后台播放日志），
	// 再走可选的 OnDecision 持久化 hook（nil 安全）。
	defer func() {
		s := e.recordDecision(req.UserID, d)
		if e.OnDecision != nil {
			e.OnDecision(s)
		}
	}()

	// User-Agent 透传：放在 defer 注册之后、GetUser 之前，
	// 确保所有返回路径（允许/拒绝/异常）的决策都带 UA。
	d.UA = req.UA

	user, err := e.deps.Users.GetUser(ctx, req.UserID)
	if err != nil {
		return d, fmt.Errorf("engine: get user %q: %w", req.UserID, err)
	}

	// ① 并发策略
	t0 := now()
	release, ok := e.acquire(user)
	step := StepTrace{Step: "concurrency", Duration: now().Sub(t0)}
	if !ok {
		limit := e.resolveLimit(user)
		step.Branch = "denied"
		step.Detail = fmt.Sprintf("template=%s limit=%d", user.Template, limit)
		d.Steps = append(d.Steps, step)
		d.Allowed, d.DenyReason, d.Branch = false, "concurrency_limit", BranchDeniedConcurrency
		e.logf("error", "[普通模式]并发拒绝 用户：%s 原因：%s", user.ID, d.DenyReason)
		return d, nil
	}
	d.Release = release
	step.Branch = "pass"
	step.Detail = fmt.Sprintf("template=%s", user.Template)
	d.Steps = append(d.Steps, step)

	// ② 定服务账号（本地路由；缓存 key 需要账号，先做这一步，见 README 设计决策）
	t0 = now()
	var accountID, routeBranch string
	var ownDrv DriveClient
	switch user.Mode {
	case ModeOwn115:
		ownDrv = e.ownDrive(user.ID)
		if ownDrv == nil {
			d.Steps = append(d.Steps, StepTrace{Step: "resolve_account", Branch: "missing", Duration: now().Sub(t0), Detail: "user has no bound 115 drive"})
			d.Allowed, d.DenyReason, d.Branch = false, "no_own_drive", BranchDeniedNoOwnDrive
			return d, nil
		}
		accountID, routeBranch = ownDrv.AccountID(), "own"
	case ModePool:
		var rerr error
		accountID, routeBranch, rerr = e.poolRoute(ctx, user.ID)
		if rerr != nil {
			d.Steps = append(d.Steps, StepTrace{Step: "resolve_account", Branch: "error", Duration: now().Sub(t0), Detail: rerr.Error()})
			d.Allowed, d.DenyReason, d.Branch = false, "pool_exhausted", BranchDeniedPoolFull
			return d, nil
		}
	default:
		d.Steps = append(d.Steps, StepTrace{Step: "resolve_account", Branch: "error", Duration: now().Sub(t0), Detail: "unknown mode " + user.Mode})
		d.Allowed, d.DenyReason, d.Branch = false, "unknown_mode", BranchDeniedUnknownMode
		return d, nil
	}
	d.AccountID = accountID
	d.Steps = append(d.Steps, StepTrace{Step: "resolve_account", Branch: routeBranch, Duration: now().Sub(t0), Detail: "account=" + accountID})
	client := req.Client
	if client == "" {
		client = "-"
	}
	e.logf("play", "收到请求 用户：%s(%s) [%s] | 文件：%s | 大小：%s",
		user.ID, accountID, client, req.FileName, formatSize(req.FileSize))

	// ③ 直链缓存（TTL 10min）
	t0 = now()
	if url, hit := e.getCache(accountID, req.FileSHA1); hit {
		d.Steps = append(d.Steps, StepTrace{Step: "cache", Branch: "hit", Duration: now().Sub(t0)})
		d.Allowed, d.Branch, d.DirectURL = true, BranchCacheHit, url
		e.logf("play", "[普通模式]缓存命中直连：%s -> %s -> %s", user.ID, accountID, req.FileName)
		e.bumpStat(user.ID, true)
		return d, nil
	}
	d.Steps = append(d.Steps, StepTrace{Step: "cache", Branch: "miss", Duration: now().Sub(t0)})

	// ④ 模式链路
	if user.Mode == ModeOwn115 {
		err = e.decideOwn115(ctx, &d, user, req, ownDrv)
	} else {
		err = e.decidePool(ctx, &d, user, req, accountID)
	}
	if err != nil {
		e.logf("error", "播放决策异常 用户：%s 文件：%s 错误：%v", user.ID, req.FileName, err)
		return d, err
	}
	d.Allowed = true
	return d, nil
}

// trace 辅助：记录一步并返回本步耗时（供日志打印毫秒数）。
func (e *Engine) traceStep(d *Decision, t0 time.Time, name, branch, detail string) time.Duration {
	dur := e.cfg.Now().Sub(t0)
	d.Steps = append(d.Steps, StepTrace{Step: name, Branch: branch, Duration: dur, Detail: detail})
	return dur
}

// finish 决策成功收尾：写缓存、记播放记录、统计成功。
// RecordPlayback 失败不影响本次播放（它只服务未来的 STEP2），记入 trace。
func (e *Engine) finish(ctx context.Context, d *Decision, user User, req PlaybackRequest, branch, url string) {
	e.setCache(d.AccountID, req.FileSHA1, url)
	if err := e.deps.Records.RecordPlayback(ctx, user.ID, req.FileSHA1); err != nil {
		d.Steps = append(d.Steps, StepTrace{Step: "record_playback", Branch: "error", Detail: err.Error()})
	}
	e.bumpStat(user.ID, true)
	d.Branch, d.DirectURL = branch, url
}

// failStat 在取直链/秒传失败时记一次失败统计。
func (e *Engine) failStat(userID string) { e.bumpStat(userID, false) }

// decideOwn115：115 模式三步链路（SPEC §12）。
func (e *Engine) decideOwn115(ctx context.Context, d *Decision, user User, req PlaybackRequest, drv DriveClient) error {
	now := e.cfg.Now
	short := shortSHA1(req.FileSHA1)

	// STEP1：自有盘 SHA1 探测（~50ms，1 次 API）
	t0 := now()
	e.logf("play", "正在执行自有盘 SHA1 探测 (%s | SHA1: %s...)", drv.AccountID(), short)
	hit, err := drv.ProbeSHA1(ctx, req.FileSHA1)
	if err != nil {
		e.traceStep(d, t0, "probe_own", "error", err.Error())
		e.failStat(user.ID)
		return fmt.Errorf("engine: probe own drive: %w", err)
	}
	if hit {
		e.traceStep(d, t0, "probe_own", "hit", "")
		e.logf("play", "[自备网盘模式]自有盘 SHA1 探测命中 | 用户：%s(%s)", user.ID, drv.AccountID())
		t1 := now()
		url, err := drv.DirectURL(ctx, req.FileSHA1)
		if err != nil {
			e.traceStep(d, t1, "direct_url", "error", err.Error())
			e.failStat(user.ID)
			return fmt.Errorf("engine: direct url: %w", err)
		}
		ms := e.traceStep(d, t1, "direct_url", "ok", "").Milliseconds()
		e.logf("play", "[自备网盘模式]直连成功：%s -> %s网盘 -> %s | 耗时：%dms", user.ID, user.ID, req.FileName, ms)
		e.finish(ctx, d, user, req, BranchOwnDriveHit, url)
		return nil
	}
	e.traceStep(d, t0, "probe_own", "miss", "")
	e.logf("play", "[自备网盘模式]自有盘 SHA1 探测失败，文件不存在(%s | SHA1: %s...)", drv.AccountID(), short)

	// STEP2：播放记录库 → 用户间秒传（本地毫秒级，零网盘 API 消耗）
	t0 = now()
	other, found, err := e.deps.Records.FindRecentPlayer(ctx, req.FileSHA1, user.ID)
	if err != nil {
		e.traceStep(d, t0, "p2p_lookup", "error", err.Error())
		return fmt.Errorf("engine: playback record lookup: %w", err)
	}
	if found {
		e.traceStep(d, t0, "p2p_lookup", "hit", "recent_player="+other)
		t1 := now()
		dir, err := drv.RapidTransfer(ctx, req.FileSHA1, req.FileName, req.FileSize)
		if err != nil {
			e.traceStep(d, t1, "rapid_transfer", "error", "src=p2p:"+other+" "+err.Error())
			e.failStat(user.ID)
			return fmt.Errorf("engine: p2p rapid transfer: %w", err)
		}
		ms := e.traceStep(d, t1, "rapid_transfer", "ok", fmt.Sprintf("src=p2p:%s dir=%s", other, dir)).Milliseconds()
		e.logf("play", "[自备网盘模式]秒传成功：p2p:%s -> %s(%s) | 目录：%s | 文件：%s | 耗时：%dms",
			other, user.ID, drv.AccountID(), dir, req.FileName, ms)
		t1 = now()
		url, err := drv.DirectURL(ctx, req.FileSHA1)
		if err != nil {
			e.traceStep(d, t1, "direct_url", "error", err.Error())
			e.failStat(user.ID)
			return fmt.Errorf("engine: direct url: %w", err)
		}
		ms = e.traceStep(d, t1, "direct_url", "ok", "").Milliseconds()
		e.logf("play", "[自备网盘模式]直连成功：%s -> %s网盘 -> %s | 耗时：%dms", user.ID, user.ID, req.FileName, ms)
		e.finish(ctx, d, user, req, BranchP2PRapid, url)
		return nil
	}
	e.traceStep(d, t0, "p2p_lookup", "miss", "")

	// STEP3：源盘兜底（100% 可用）
	t1 := now()
	e.logf("play", "正在尝试源网盘秒传：%s -> %s(%s)", e.cfg.SeedAccountID, user.ID, drv.AccountID())
	dir, err := drv.RapidTransfer(ctx, req.FileSHA1, req.FileName, req.FileSize)
	if err != nil {
		e.traceStep(d, t1, "rapid_transfer", "error", "src=seed:"+e.cfg.SeedAccountID+" "+err.Error())
		e.failStat(user.ID)
		return fmt.Errorf("engine: seed rapid transfer: %w", err)
	}
	ms := e.traceStep(d, t1, "rapid_transfer", "ok", fmt.Sprintf("src=seed:%s dir=%s", e.cfg.SeedAccountID, dir)).Milliseconds()
	e.logf("play", "[自备网盘模式]秒传成功：%s -> %s(%s) | 目录：%s | 文件：%s | 耗时：%dms",
		e.cfg.SeedAccountID, user.ID, drv.AccountID(), dir, req.FileName, ms)
	t1 = now()
	url, err := drv.DirectURL(ctx, req.FileSHA1)
	if err != nil {
		e.traceStep(d, t1, "direct_url", "error", err.Error())
		e.failStat(user.ID)
		return fmt.Errorf("engine: direct url: %w", err)
	}
	ms = e.traceStep(d, t1, "direct_url", "ok", "").Milliseconds()
	e.logf("play", "[自备网盘模式]直连成功：%s -> %s网盘 -> %s | 耗时：%dms", user.ID, user.ID, req.FileName, ms)
	e.finish(ctx, d, user, req, BranchSeedFallback, url)
	return nil
}

// decidePool：池模式链路（SPEC §9/§12，普通模式）。
func (e *Engine) decidePool(ctx context.Context, d *Decision, user User, req PlaybackRequest, accountID string) error {
	now := e.cfg.Now
	drv := e.deps.PoolDrv[accountID]
	if drv == nil {
		e.failStat(user.ID)
		return fmt.Errorf("engine: no drive client for pool account %q", accountID)
	}

	// 全域 SHA1 探测
	short := shortSHA1(req.FileSHA1)
	t0 := now()
	e.logf("play", "正在执行全域 SHA1 探测 (%s | SHA1: %s...)", accountID, short)
	hit, err := drv.ProbeSHA1(ctx, req.FileSHA1)
	if err != nil {
		e.traceStep(d, t0, "probe_pool", "error", err.Error())
		e.failStat(user.ID)
		return fmt.Errorf("engine: probe pool drive: %w", err)
	}
	if hit {
		e.traceStep(d, t0, "probe_pool", "hit", "")
		e.logf("play", "[普通模式]全域 SHA1 探测命中 | 用户：%s(%s)", user.ID, accountID)
		t1 := now()
		url, err := drv.DirectURL(ctx, req.FileSHA1)
		if err != nil {
			e.traceStep(d, t1, "direct_url", "error", err.Error())
			e.failStat(user.ID)
			return fmt.Errorf("engine: direct url: %w", err)
		}
		ms := e.traceStep(d, t1, "direct_url", "ok", "").Milliseconds()
		e.logf("play", "[普通模式]直连成功：%s -> %s网盘 -> %s | 耗时：%dms", user.ID, user.ID, req.FileName, ms)
		// 探测命中不是秒传，不续期 24h 锁（让冷文件自然过期进入 GC，见 README）。
		e.finish(ctx, d, user, req, BranchPoolProbeHit, url)
		return nil
	}
	e.traceStep(d, t0, "probe_pool", "miss", "")
	e.logf("play", "[普通模式]全域 SHA1 探测失败，文件不存在(%s | SHA1: %s...)", accountID, short)

	// 神盾快速查库（按 sha1）
	t0 = now()
	sr, err := e.deps.Shield.SearchBySHA1(ctx, req.FileSHA1)
	if err != nil {
		e.traceStep(d, t0, "shield_search", "error", err.Error())
		return fmt.Errorf("engine: shield search: %w", err)
	}
	if sr.Found {
		e.traceStep(d, t0, "shield_search", "hit", "share_code="+sr.ShareCode)
		e.logf("play", "[普通模式]神盾快速查库命中该资源 (分享码：%s)", sr.ShareCode)
		t1 := now()
		if err := e.deps.Shield.TransferToPool(ctx, sr.Slug, e.cfg.ReceiveDir); err != nil {
			e.traceStep(d, t1, "shield_transfer", "error", err.Error())
			e.failStat(user.ID)
			return fmt.Errorf("engine: shield transfer to pool: %w", err)
		}
		e.traceStep(d, t1, "shield_transfer", "ok", "slug="+sr.Slug+" dir="+e.cfg.ReceiveDir)
		e.renewLock(user.ID, accountID)
		t1 = now()
		url, err := drv.DirectURL(ctx, req.FileSHA1)
		if err != nil {
			e.traceStep(d, t1, "direct_url", "error", err.Error())
			e.failStat(user.ID)
			return fmt.Errorf("engine: direct url: %w", err)
		}
		ms := e.traceStep(d, t1, "direct_url", "ok", "").Milliseconds()
		e.logf("play", "[普通模式]直连成功：%s -> %s网盘 -> %s | 耗时：%dms", user.ID, user.ID, req.FileName, ms)
		e.finish(ctx, d, user, req, BranchPoolShieldHit, url)
		return nil
	}
	e.traceStep(d, t0, "shield_search", "miss", "")
	e.logf("play", "[普通模式]神盾快速查库未命中该资源 (文件：%s)", req.FileName)

	// 神盾未命中：后台触发主动搜索（尽力而为，不阻塞）+ 前台源网盘秒传兜底
	if req.TMDBID != "" {
		t1 := now()
		if berr := e.deps.Shield.TriggerBackgroundSearch(ctx, req.TMDBID, req.MediaType); berr != nil {
			e.traceStep(d, t1, "shield_background_search", "error", berr.Error())
		} else {
			e.traceStep(d, t1, "shield_background_search", "triggered", "tmdb_id="+req.TMDBID)
		}
	}
	t1 := now()
	e.logf("play", "正在尝试源网盘秒传：%s -> %s(%s)", e.cfg.SeedAccountID, user.ID, accountID)
	dir, err := drv.RapidTransfer(ctx, req.FileSHA1, req.FileName, req.FileSize)
	if err != nil {
		e.traceStep(d, t1, "rapid_transfer", "error", "src=seed:"+e.cfg.SeedAccountID+" "+err.Error())
		e.failStat(user.ID)
		return fmt.Errorf("engine: pool seed rapid transfer: %w", err)
	}
	ms := e.traceStep(d, t1, "rapid_transfer", "ok", fmt.Sprintf("src=seed:%s dir=%s", e.cfg.SeedAccountID, dir)).Milliseconds()
	e.logf("play", "[普通模式]秒传成功：%s -> %s(%s) | 目录：%s | 文件：%s | 耗时：%dms",
		e.cfg.SeedAccountID, user.ID, accountID, dir, req.FileName, ms)
	e.renewLock(user.ID, accountID)
	t1 = now()
	url, err := drv.DirectURL(ctx, req.FileSHA1)
	if err != nil {
		e.traceStep(d, t1, "direct_url", "error", err.Error())
		e.failStat(user.ID)
		return fmt.Errorf("engine: direct url: %w", err)
	}
	ms = e.traceStep(d, t1, "direct_url", "ok", "").Milliseconds()
	e.logf("play", "[普通模式]直连成功：%s -> %s网盘 -> %s | 耗时：%dms", user.ID, user.ID, req.FileName, ms)
	e.finish(ctx, d, user, req, BranchPoolSeedFallback, url)
	return nil
}
