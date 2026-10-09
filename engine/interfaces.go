package engine

import "context"

// DriveClient 抽象对单个 115 网盘账号的操作。
//
// 真实实现（模块3：115 适配器）对接 115 非官方 API；
// memory 子包提供 in-memory 假实现供测试与演示。
type DriveClient interface {
	// AccountID 标识这个网盘账号，如 "115小2"。
	AccountID() string
	// ProbeSHA1 = 全域 SHA1 探测：该账号的网盘里是否存在此 SHA1 的文件。
	ProbeSHA1(ctx context.Context, sha1 string) (bool, error)
	// RapidTransfer = 秒传：按 sha1 把文件秒传进该账号的接收目录。
	// 115 的秒传基于服务端全局去重，只需要哈希，不需要源盘在线；
	// fileSize 必填（115 initupload 接口要求），调用方必须从 PlaybackRequest.FileSize 透传。
	// 返回文件落盘目录（如 "/最近接收"）。
	RapidTransfer(ctx context.Context, sha1, fileName string, fileSize int64) (string, error)
	// DirectURL 取该账号下该文件的一条新鲜直链（115 直链有时效）。
	DirectURL(ctx context.Context, sha1 string) (string, error)
}

// ShieldResult 是 NextFind /shield/search 的查询结果。
type ShieldResult struct {
	Found     bool
	ShareCode string // 115 分享码
	Slug      string // nextfind://... 内部资源标识
}

// ShieldClient 抽象 NextFind OpenAPI 的神盾相关接口。
//
// 真实实现（后续模块）调用 NextFind /api/openapi（X-API-Key 鉴权）。
type ShieldClient interface {
	// SearchBySHA1 = 神盾快速查库：按 sha1 查分享码。
	SearchBySHA1(ctx context.Context, sha1 string) (ShieldResult, error)
	// TriggerBackgroundSearch = 神盾主动搜索：按 tmdb_id 后台异步找资源，
	// 不阻塞本次播放。调用失败视为尽力而为，不应导致播放失败。
	TriggerBackgroundSearch(ctx context.Context, tmdbID, mediaType string) error
	// TransferToPool = 一键转存：把神盾资源（slug）转存进池账号的目标目录。
	TransferToPool(ctx context.Context, slug, targetFolder string) error
}

// PlaybackRecordStore 是"播放记录库"：sha1 -> 播过该文件的用户。
// 支撑 115 模式 STEP2（用户间秒传，本地毫秒级查询，零网盘 API 消耗）。
type PlaybackRecordStore interface {
	// RecordPlayback 记录一次播放（幂等性由实现保证）。
	RecordPlayback(ctx context.Context, userID, sha1 string) error
	// FindRecentPlayer 返回最近播过该文件的其他用户（排除 excludeUserID）。
	FindRecentPlayer(ctx context.Context, sha1, excludeUserID string) (string, bool, error)
}

// UserStore 提供 NextEmby 侧的用户记录。
type UserStore interface {
	GetUser(ctx context.Context, userID string) (User, error)
}

// UserUpdater 是 UserStore 的可选扩展：更新用户记录（模式/模板）。
// memory 实现支持；DB 实现待接入。调用方用类型断言探测，
// 断言失败应视为未实现（管理后台返回 501）。
type UserUpdater interface {
	UpdateUser(ctx context.Context, u User) error
}

// UserLister 是 UserStore 的可选扩展：枚举全部用户（管理后台用）。
type UserLister interface {
	ListUsers(ctx context.Context) ([]User, error)
}

// PoolStore 提供分布式池的账号列表与健康状态。
// （24h 路由锁的记账在 Engine 内部，见 engine.go。）
type PoolStore interface {
	ListAccounts(ctx context.Context) ([]PoolAccount, error)
	// SetHealthy 供将来的 API 监控接入：把被限流/异常的账号摘除。
	SetHealthy(ctx context.Context, accountID string, healthy bool) error
}
