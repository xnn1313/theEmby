package adapter115

// 本文件定义 115 适配器的结构化错误。
//
// 设计目标：让上层引擎（engine）能用 errors.As 做分支决策，而不必解析错误字符串：
//   - 秒传未命中 -> IsRapidMiss -> 走"源盘兜底 / 神盾"分支
//   - Cookie 失效 -> IsAuthError -> 摘除账号 / 告警换 Cookie
//   - 取直链失败 -> IsRetryable 决定是否重试
//
// 所有错误都携带 AccountID / Op / Elapsed 等结构化字段，方便打日志与观测。
// 注意：任何错误里都不得包含 cookie 内容。

import (
	"errors"
	"fmt"
	"time"
)

// RapidMissReason 说明秒传未命中的具体原因。
type RapidMissReason string

const (
	// RapidMissNotInPool 文件不在 115 服务端去重池，需要真实上传
	// （适配器没有文件字节，只能视为未命中）。
	RapidMissNotInPool RapidMissReason = "not_in_dedup_pool"
	// RapidMissSignCheck 115 要求 range 签名证明（status 7），
	// 纯哈希秒传无文件字节，无法完成证明。
	RapidMissSignCheck RapidMissReason = "sign_check_required"
	// RapidMissNeedFileSize 缺少文件大小，无法调用秒传接口
	// （115 initupload 的 filesize 是必填字段）。
	// 调用方应通过 WithSizeResolver 提供 sha1 -> 大小的解析。
	RapidMissNeedFileSize RapidMissReason = "need_file_size"
)

// RapidMissError 表示秒传未命中。这不是 I/O 故障，而是正常业务分支：
// 引擎收到它应走"源盘兜底 / 神盾查库"分支，而不是记失败统计。
type RapidMissError struct {
	AccountID string
	SHA1      string
	Reason    RapidMissReason
	Elapsed   time.Duration
}

func (e *RapidMissError) Error() string {
	return fmt.Sprintf("adapter115: rapid upload miss account=%s sha1=%.12s… reason=%s elapsed=%s",
		e.AccountID, e.SHA1, e.Reason, e.Elapsed.Round(time.Millisecond))
}

// IsRapidMiss 判断 err 是否为秒传未命中。
func IsRapidMiss(err error) bool {
	var t *RapidMissError
	return errors.As(err, &t)
}

// AuthError 表示 115 会话认证失败（Cookie 失效 / 未登录 / 被踢下线）。
// 收到它应把该账号标记不健康并告警，而不是重试。
type AuthError struct {
	AccountID string
	Op        string // 哪个操作触发的，如 "login_check" / "rapid_upload"
	Err       error  // 底层错误（已确保不含 cookie）
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("adapter115: auth failed account=%s op=%s: %v", e.AccountID, e.Op, e.Err)
}

func (e *AuthError) Unwrap() error { return e.Err }

// IsAuthError 判断 err 是否为认证失败。
func IsAuthError(err error) bool {
	var t *AuthError
	return errors.As(err, &t)
}

// FileNotFoundError 表示账号内找不到该 SHA1 的文件。
// 对 DirectURL 而言这是确定性失败，不应重试。
type FileNotFoundError struct {
	AccountID string
	SHA1      string
}

func (e *FileNotFoundError) Error() string {
	return fmt.Sprintf("adapter115: file not found account=%s sha1=%.12s…", e.AccountID, e.SHA1)
}

// IsFileNotFound 判断 err 是否为文件不存在。
func IsFileNotFound(err error) bool {
	var t *FileNotFoundError
	return errors.As(err, &t)
}

// DirectURLError 表示取直链失败，Retryable 区分是否值得重试。
//
// 可重试：网络抖动、超时、疑似限流（429/503/502/504、"try again" 等）。
// 不可重试：文件不存在、无权限、参数错误、认证失败（认证失败会先被转成 AuthError）。
type DirectURLError struct {
	AccountID string
	SHA1      string
	Retryable bool
	Elapsed   time.Duration
	Err       error
}

func (e *DirectURLError) Error() string {
	return fmt.Sprintf("adapter115: direct url failed account=%s sha1=%.12s… retryable=%v elapsed=%s: %v",
		e.AccountID, e.SHA1, e.Retryable, e.Elapsed.Round(time.Millisecond), e.Err)
}

func (e *DirectURLError) Unwrap() error { return e.Err }

// IsRetryable 判断 err 是否值得重试。
// FileNotFoundError / RapidMissError / AuthError 恒为 false。
func IsRetryable(err error) bool {
	var t *DirectURLError
	if errors.As(err, &t) {
		return t.Retryable
	}
	return false
}
