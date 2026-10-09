package engine

import (
	"fmt"
	"strings"
	"time"
)

// 播放模式：per-user 开关（SPEC §4）。
const (
	// ModeOwn115 秒传到用户自己的 115 网盘。
	ModeOwn115 = "115"
	// ModePool 秒传到分布式池（运营方多账号网盘池，负载均衡）。
	ModePool = "pool"
)

// 决策主分支（Decision.Branch 的取值）。
const (
	BranchCacheHit         = "cache_hit"            // 10min 直链缓存命中
	BranchOwnDriveHit      = "own_drive_hit"        // 115模式 STEP1：自有盘 SHA1 命中
	BranchP2PRapid         = "p2p_rapid"            // 115模式 STEP2：用户间秒传
	BranchSeedFallback     = "seed_fallback"        // 115模式 STEP3：源盘兜底
	BranchPoolProbeHit     = "pool_probe_hit"       // 池模式：全域 SHA1 探测命中
	BranchPoolShieldHit    = "pool_shield_transfer" // 池模式：神盾命中，转存进池
	BranchPoolSeedFallback = "pool_seed_fallback"   // 池模式：神盾未命中，源网盘秒传

	BranchDeniedConcurrency = "denied_concurrency"
	BranchDeniedNoOwnDrive  = "denied_no_own_drive"
	BranchDeniedPoolFull    = "denied_pool_exhausted"
	BranchDeniedUnknownMode = "denied_unknown_mode"
)

// PlaybackRequest 是一次播放决策请求。
type PlaybackRequest struct {
	UserID    string // NextEmby 侧用户，如 "lzy"
	FileSHA1  string // 文件 SHA1（hex，如日志里的 A2375D8E...）
	FileName  string // 如 "去有风的地方.Meet Yourself.2023.S01E04.mp4"
	FileSize  int64
	Client    string // 客户端标识，如 "Filmly" / "VidHub"
	TMDBID    string // 上游 Emby 的 tmdb_id（神盾主动搜索用，可空）
	MediaType string // "movie" / "tv"
	UA        string // HTTP User-Agent 原始值（透传进决策与日志）
}

// StepTrace 记录决策链中某一步的分支与耗时，方便以后打日志
// （对标用户提供的真实播放日志格式）。
type StepTrace struct {
	Step     string        // 步骤名，如 "concurrency" / "cache" / "probe_own"
	Branch   string        // 分支，如 "pass" / "denied" / "hit" / "miss"
	Duration time.Duration // 本步耗时
	Detail   string        // 补充信息，如 "src=seed:115大 dir=/最近接收"
}

// Decision 是一次播放决策的结构化结果。
type Decision struct {
	Allowed    bool        // 是否允许播放
	DenyReason string      // 拒绝原因，如 "concurrency_limit"（允许时为空）
	Branch     string      // 走的主分支（见 Branch* 常量）
	DirectURL  string      // 决策出的直链（缓存命中或现取）
	AccountID  string      // 实际服务的 115 账号（用户自有盘或池账号如 "115小2"）
	UA         string      // 请求的 HTTP User-Agent 原始值（由 Handle 从请求透传）
	Steps      []StepTrace // 每步分支 + 耗时
	Release    func()      // 播放结束时调用以释放会话；拒绝时为 nil
}

// Summary 输出一行人类可读的摘要，方便日志打印。
func (d Decision) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "allowed=%v branch=%s account=%s", d.Allowed, d.Branch, d.AccountID)
	if d.DenyReason != "" {
		fmt.Fprintf(&sb, " deny=%s", d.DenyReason)
	}
	if d.DirectURL != "" {
		fmt.Fprintf(&sb, " url=%s", d.DirectURL)
	}
	sb.WriteString(" steps=[")
	for i, s := range d.Steps {
		if i > 0 {
			sb.WriteString(" ")
		}
		fmt.Fprintf(&sb, "%s:%s", s.Step, s.Branch)
		if s.Detail != "" {
			fmt.Fprintf(&sb, "(%s)", s.Detail)
		}
	}
	sb.WriteString("]")
	return sb.String()
}

// User 是 NextEmby 侧的用户记录。
type User struct {
	ID        string
	Mode      string // ModeOwn115 / ModePool
	Template  string // 并发策略模板，如 "vip"
	ExpiresAt int64  // 过期时间（unix 秒）；0=永不过期
	Banned    bool   // 是否被封禁
	Remark    string // 备注
}

// Template 是并发策略模板（读 DB templates 表）。
type Template struct {
	Name          string
	MaxConcurrent int
	MaxDevices    int
}

// PoolAccount 是分布式池中的一个 115 服务账号（如 "115小2"）。
type PoolAccount struct {
	ID       string
	Healthy  bool
	MaxUsers int // 锁定用户上限；<=0 时按默认 4 处理（SPEC：3~4）
}

// MaxUsersOrDefault 返回生效的用户上限。
func (a PoolAccount) MaxUsersOrDefault() int {
	if a.MaxUsers <= 0 {
		return 4
	}
	return a.MaxUsers
}

// DecisionSummary 是决策的轻量快照，供管理后台播放日志的 ring buffer 使用
// （复用 Decision 的 StepTrace；不含 Release 等运行时字段）。
type DecisionSummary struct {
	At         time.Time
	UserID     string
	Branch     string
	Allowed    bool
	DenyReason string
	AccountID  string
	UA         string
	Steps      []StepTrace
}

// ElapsedMs 返回各步骤耗时之和（毫秒）。
func (s DecisionSummary) ElapsedMs() int64 {
	var d time.Duration
	for _, st := range s.Steps {
		d += st.Duration
	}
	return d.Milliseconds()
}

// StatPair 是单个用户的直连成功/失败计数。
type StatPair struct {
	OK   int64
	Fail int64
}

// PoolLock 是 24h 路由锁的一条只读记录。
type PoolLock struct {
	UserID    string
	AccountID string
	ExpiresAt time.Time
}
