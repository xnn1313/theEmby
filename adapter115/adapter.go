// Package adapter115 是 115 网盘适配器（NextEmby 复刻项目的模块3），
// 实现 engine.DriveClient 接口，对接单个 115 网盘账号。
//
// 架构：
//
//		engine.DriveClient ──► adapter115.Client ──► wireClient ──► 115 非官方 API
//		                     （业务决策/日志）      （协议窄缝）
//
//	  - wireClient 的真实实现是 realWire（wire.go），基于 115driver；
//	  - 单元测试用 fakeWire 注入任意分支，无需真实 115 账号；
//	  - cookie 由调用方传入构造器，只用于建会话，绝不记入日志或错误。
//
// 日志风格对标 SPEC 里的真实播放日志，例如：
//
//	✅[115小2]秒传成功 | 文件: 去有风的地方….mp4 | 目录: /最近接收 | 耗时: 1377ms
//	🔄[115小2]全域SHA1探测: 未命中 sha1=A2375D8E… | 耗时: 96ms
package adapter115

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"nextemby-replay/engine"
)

// DefaultReceiveDir 是秒传落盘目录（SPEC §9/§12："/最近接收"）。
const DefaultReceiveDir = "/最近接收"

// SizeResolver 按 sha1 解析文件大小（字节）。
// 115 的秒传接口（initupload）要求 filesize 必填；引擎的 PlaybackRequest 自带
// FileSize，接入时应把 resolver 接到资源索引 / 请求上下文上。
// 返回 ok=false 表示未知大小，此时 RapidTransfer 会返回 RapidMissNeedFileSize。
type SizeResolver func(ctx context.Context, sha1 string) (size int64, ok bool)

// Option 配置 Client。
type Option func(*Client)

// WithReceiveDir 覆盖秒传落盘目录（默认 "/最近接收"）。
func WithReceiveDir(dir string) Option {
	return func(c *Client) { c.receiveDir = dir }
}

// WithSizeResolver 注入 sha1 -> 文件大小的解析器（秒传必需）。
func WithSizeResolver(fn SizeResolver) Option {
	return func(c *Client) { c.sizeOf = fn }
}

// WithLogger 注入日志输出（默认打到 stderr）。
func WithLogger(l *log.Logger) Option {
	return func(c *Client) { c.log = l }
}

// WithWire 注入 wireClient 实现（测试用；生产代码不要用）。
func WithWire(w wireClient) Option {
	return func(c *Client) { c.wire = w }
}

// Client 是单个 115 账号的适配器，实现 engine.DriveClient。
// 一个实例 = 一个已登录会话（115大 / 115小N / 用户自有盘各一个实例）。
// 方法都是并发安全的。
type Client struct {
	accountID  string
	wire       wireClient
	receiveDir string
	sizeOf     SizeResolver
	log        *log.Logger

	mu         sync.Mutex
	receiveCID string // "/最近接收" 解析出的 cid，懒加载缓存
}

// newClient 组装 Client（不做网络 I/O）。
func newClient(accountID string, w wireClient, opts ...Option) *Client {
	c := &Client{
		accountID:  accountID,
		wire:       w,
		receiveDir: DefaultReceiveDir,
		log:        log.New(os.Stderr, "", log.LstdFlags),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// New 创建并登录一个 115 会话。
//
// accountID: 账号标识，如 "115大" / "115小2"（仅用于日志与错误，不发往 115）。
// cookie: 115 登录 Cookie（如 "UID=…;CID=…;SEID=…;KID=…"），由调用方传入并自行保管；
//
//	构造时做一次 LoginCheck，fail-fast，Cookie 失效直接返回 *AuthError。
//
// 注意：cookie 绝不会出现在日志或返回的错误里。
func New(ctx context.Context, accountID, cookie string, opts ...Option) (*Client, error) {
	w, err := newRealWire(cookie)
	if err != nil {
		var ae *AuthError
		if errors.As(err, &ae) {
			ae.AccountID = accountID
		}
		return nil, err
	}
	c := newClient(accountID, w, opts...)
	// 构造时做一次 LoginCheck，fail-fast：Cookie 失效直接返回 *AuthError，
	// 避免带着无效会话工作。
	if err := c.CheckSession(ctx); err != nil {
		return nil, err
	}
	c.logf("✅[%s]会话建立: 115 登录态校验通过", accountID)
	return c, nil
}

// newForTest 供单测用：跳过真实建会话，直接注入 fake wire，日志丢弃。
func newForTest(accountID string, w wireClient, opts ...Option) *Client {
	opts = append([]Option{WithLogger(log.New(io.Discard, "", 0))}, opts...)
	return newClient(accountID, w, opts...)
}

// AccountID 实现 engine.DriveClient。
func (c *Client) AccountID() string { return c.accountID }

// CheckSession 供运维/健康检查调用：校验当前 cookie 是否仍然有效。
func (c *Client) CheckSession(ctx context.Context) error {
	if err := c.wire.checkSession(ctx); err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			if ae.Op == "115api" {
				ae.Op = "check_session"
			}
			return ae
		}
		return fmt.Errorf("adapter115: check session account=%s: %w", c.accountID, err)
	}
	return nil
}

// recvCID 解析并缓存落盘目录的 cid；目录不存在则自动创建。
func (c *Client) recvCID(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.receiveCID != "" {
		cid := c.receiveCID
		c.mu.Unlock()
		return cid, nil
	}
	c.mu.Unlock()

	cid, err := c.wire.dirCIDByPath(ctx, c.receiveDir)
	if err == nil && cid != "" && cid != "0" {
		c.mu.Lock()
		c.receiveCID = cid
		c.mu.Unlock()
		return cid, nil
	}
	// 目录不存在（或解析失败）：在根目录下创建。
	name := strings.Trim(c.receiveDir, "/")
	newCID, merr := c.wire.mkdir(ctx, "0", name)
	if merr != nil {
		if ae := asAuthError(merr); ae != nil {
			ae.AccountID = c.accountID
			return "", ae
		}
		return "", fmt.Errorf("adapter115: ensure receive dir account=%s: %w", c.accountID, merr)
	}
	c.mu.Lock()
	c.receiveCID = newCID
	c.mu.Unlock()
	c.logf("📁[%s]创建目录: %s (cid=%s)", c.accountID, c.receiveDir, newCID)
	return newCID, nil
}

// findInScopes 按 sha1 找文件：先接收目录，再根目录（"全域"兜底）。
func (c *Client) findInScopes(ctx context.Context, sha1 string) (*fileMeta, error) {
	cid, err := c.recvCID(ctx)
	if err != nil {
		return nil, err
	}
	m, err := c.wire.findBySHA1(ctx, cid, sha1)
	if err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			return nil, ae
		}
		return nil, err
	}
	return m, nil
}

// ProbeSHA1 实现 engine.DriveClient：全域 SHA1 探测。
// 返回 (true, nil) = 该账号已有此文件；(false, nil) = 不存在。
func (c *Client) ProbeSHA1(ctx context.Context, sha1 string) (bool, error) {
	t0 := time.Now()
	m, err := c.findInScopes(ctx, sha1)
	el := time.Since(t0)
	if err != nil {
		c.logf("❌[%s]全域SHA1探测: 出错 sha1=%s | 耗时: %dms | err=%v", c.accountID, shortSHA1(sha1), el.Milliseconds(), err)
		return false, err
	}
	if m == nil {
		c.logf("🔄[%s]全域SHA1探测: 未命中 sha1=%s | 耗时: %dms", c.accountID, shortSHA1(sha1), el.Milliseconds())
		return false, nil
	}
	c.logf("✅[%s]全域SHA1探测: 命中 sha1=%s 文件=%s | 耗时: %dms", c.accountID, shortSHA1(sha1), m.Name, el.Milliseconds())
	return true, nil
}

// RapidTransfer 实现 engine.DriveClient：SHA1 秒传进接收目录。
// fileSize 由引擎从 PlaybackRequest.FileSize 透传（115 initupload 必填）；
// 若 fileSize<=0 则回退到 WithSizeResolver 注入的解析器，仍未知时返回
// *RapidMissError（RapidMissNeedFileSize），不碰网络。
// 成功返回落盘目录（如 "/最近接收"）；去重池未命中返回 *RapidMissError
// （引擎据此走"源盘兜底 / 神盾"分支）。
func (c *Client) RapidTransfer(ctx context.Context, sha1, fileName string, fileSize int64) (string, error) {
	t0 := time.Now()
	size := fileSize
	var ok bool
	if size <= 0 && c.sizeOf != nil {
		size, ok = c.sizeOf(ctx, sha1)
	} else {
		ok = size > 0
	}
	if !ok {
		el := time.Since(t0)
		c.logf("🔄[%s]秒传跳过: 缺少文件大小 sha1=%s | 耗时: %dms", c.accountID, shortSHA1(sha1), el.Milliseconds())
		return "", &RapidMissError{AccountID: c.accountID, SHA1: sha1, Reason: RapidMissNeedFileSize, Elapsed: el}
	}
	cid, err := c.recvCID(ctx)
	if err != nil {
		return "", err
	}
	out, err := c.wire.rapidUpload(ctx, cid, fileName, sha1, size)
	el := time.Since(t0)
	if err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			c.logf("❌[%s]秒传失败: 认证失效 文件=%s | 耗时: %dms", c.accountID, fileName, el.Milliseconds())
			return "", ae
		}
		c.logf("❌[%s]秒传失败: 文件=%s err=%v | 耗时: %dms", c.accountID, fileName, err, el.Milliseconds())
		return "", fmt.Errorf("adapter115: rapid upload account=%s: %w", c.accountID, err)
	}
	switch out {
	case rapidHit:
		c.logf("✅[%s]秒传成功 | 目录: %s | 文件: %s | 大小: %s | 耗时: %dms",
			c.accountID, c.receiveDir, fileName, formatSize(size), el.Milliseconds())
		return c.receiveDir, nil
	case rapidSignCheck:
		c.logf("🔄[%s]秒传未命中: 需range签名证明(无文件字节) 文件=%s | 耗时: %dms", c.accountID, fileName, el.Milliseconds())
		return "", &RapidMissError{AccountID: c.accountID, SHA1: sha1, Reason: RapidMissSignCheck, Elapsed: el}
	default:
		c.logf("🔄[%s]秒传未命中: 去重池无此文件 文件=%s | 耗时: %dms", c.accountID, fileName, el.Milliseconds())
		return "", &RapidMissError{AccountID: c.accountID, SHA1: sha1, Reason: RapidMissNotInPool, Elapsed: el}
	}
}

// DirectURL 实现 engine.DriveClient：取一条新鲜直链（115 直链有时效，
// 上层按 SPEC 做 10 分钟缓存，不要长期缓存返回值）。
func (c *Client) DirectURL(ctx context.Context, sha1 string) (string, error) {
	t0 := time.Now()
	m, err := c.findInScopes(ctx, sha1)
	if err != nil {
		return "", err
	}
	if m == nil {
		c.logf("❌[%s]取直链失败: 文件不存在 sha1=%s | 耗时: %dms", c.accountID, shortSHA1(sha1), time.Since(t0).Milliseconds())
		return "", &FileNotFoundError{AccountID: c.accountID, SHA1: sha1}
	}
	u, err := c.wire.downloadURL(ctx, m.PickCode)
	el := time.Since(t0)
	if err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			return "", ae
		}
		retry := isRetryableNetErr(err)
		c.logf("❌[%s]取直链失败: 文件=%s 可重试=%v err=%v | 耗时: %dms", c.accountID, m.Name, retry, err, el.Milliseconds())
		return "", &DirectURLError{AccountID: c.accountID, SHA1: sha1, Retryable: retry, Elapsed: el, Err: err}
	}
	c.logf("✅[%s]直连成功 | 文件: %s | 耗时: %dms", c.accountID, m.Name, el.Milliseconds())
	return u, nil
}

// ---------------------------------------------------------------- 扩展能力
// 以下是对 NextFind /directories 语义的直接支持，不属于 engine.DriveClient。

// DirEntry 是目录项的精简视图。
type DirEntry struct {
	CID   string
	Name  string
	IsDir bool
	Size  int64
	SHA1  string
}

// ListDir 按 cid 列出子目录（NextFind GET /directories 语义）。
func (c *Client) ListDir(ctx context.Context, cid string) ([]DirEntry, error) {
	if cid == "" {
		cid = "0"
	}
	metas, err := c.wire.listDir(ctx, cid)
	if err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			return nil, ae
		}
		return nil, fmt.Errorf("adapter115: list dir account=%s cid=%s: %w", c.accountID, cid, err)
	}
	out := make([]DirEntry, 0, len(metas))
	for _, m := range metas {
		out = append(out, DirEntry{CID: m.CID, Name: m.Name, IsDir: m.IsDir, Size: m.Size, SHA1: m.SHA1})
	}
	return out, nil
}

// Mkdir 在 parentCID 下新建目录，返回新目录 cid
// （NextFind POST /directories 语义；parentCID 为空时用根目录）。
func (c *Client) Mkdir(ctx context.Context, parentCID, name string) (string, error) {
	if parentCID == "" {
		parentCID = "0"
	}
	cid, err := c.wire.mkdir(ctx, parentCID, name)
	if err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			return "", ae
		}
		return "", fmt.Errorf("adapter115: mkdir account=%s parent=%s name=%s: %w", c.accountID, parentCID, name, err)
	}
	c.logf("📁[%s]创建目录: %s (cid=%s, parent=%s)", c.accountID, name, cid, parentCID)
	return cid, nil
}

// DirCID 按路径解析 cid（如 "/最近接收"）。
func (c *Client) DirCID(ctx context.Context, path string) (string, error) {
	cid, err := c.wire.dirCIDByPath(ctx, path)
	if err != nil {
		if ae := asAuthError(err); ae != nil {
			ae.AccountID = c.accountID
			return "", ae
		}
		return "", fmt.Errorf("adapter115: resolve dir account=%s path=%s: %w", c.accountID, path, err)
	}
	return cid, nil
}

// FileSHA1ByPath 按 115 网盘路径实时取文件的 SHA1（网关指纹解析用）。
//
// 设计说明（2026-10-09 用户确认）：不预扫建快照；播放时把 Emby 路径经映射规则
// 换算成 115 路径后，实时调 115 文件列表 API 取 sha1 字段。
// 路径不存在返回普通 error（网关视为指纹 miss，走优雅降级）。
func (c *Client) FileSHA1ByPath(ctx context.Context, path string) (string, error) {
	t0 := time.Now()
	p := strings.Trim(path, "/")
	dir, name := "/", p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		dir, name = "/"+p[:i], p[i+1:]
	}
	cid, err := c.DirCID(ctx, dir)
	if err != nil {
		return "", err
	}
	entries, err := c.ListDir(ctx, cid)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir && e.Name == name && e.SHA1 != "" {
			c.logf("🔍[%s]按路径取SHA1: %s → %s | 耗时: %dms",
				c.accountID, path, shortSHA1(e.SHA1), time.Since(t0).Milliseconds())
			return e.SHA1, nil
		}
	}
	return "", fmt.Errorf("adapter115: file not found account=%s path=%s", c.accountID, path)
}

// ---------------------------------------------------------------- 内部工具

func (c *Client) logf(format string, args ...interface{}) {
	c.log.Printf(format, args...)
}

// shortSHA1 取 sha1 前 8 位用于日志（对标真实日志里的 "SHA1: A2375D8E..."）。
func shortSHA1(sha1 string) string {
	s := strings.ToUpper(sha1)
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}

func formatSize(n int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2fGB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.2fMB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.2fKB", float64(n)/KB)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// 编译期断言：Client 实现 engine.DriveClient。
var _ engine.DriveClient = (*Client)(nil)
