package adapter115

// 本文件定义 115 线路协议的最小能力子集（wireClient），以及基于
// github.com/SheltonZhu/115driver 的真实实现（realWire）。
//
// 为什么需要这一层：
//  1. 115 的秒传 / 取直链接口是加密协议（ECDH 握手、m115 加密），
//     自行实现易错且难跟进 115 的协议变更；115driver 是 alist 115 驱动
//     实际采用的同一实现（MIT 协议），直接依赖它是最稳的选择。
//  2. 把"业务决策"（adapter.go）与"线路协议"隔开，单元测试可以用
//     fakeWire 注入任意分支，而不必复刻 115 的加密协议去搭 mock 服务端。
//
// 安全：cookie 只在构造 realWire 时解析，绝不写入任何日志 / 错误。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	driver115 "github.com/SheltonZhu/115driver/pkg/driver"
)

// fileMeta 是 115 文件的最小元数据子集，屏蔽 115driver 的具体类型。
type fileMeta struct {
	FileID   string
	Name     string
	Size     int64
	SHA1     string // 115 返回大写 hex；比对时忽略大小写
	PickCode string
	CID      string // 所在目录 cid
	IsDir    bool
}

// rapidOutcome 是秒传接口（initupload）的判定结果。
type rapidOutcome int

const (
	rapidHit       rapidOutcome = iota // 秒传成功：服务端去重命中，文件已落盘
	rapidMiss                          // 去重池未命中，需要真实上传
	rapidSignCheck                     // 115 要求 range 签名证明（需要文件字节）
)

// wireClient 是 115 线路协议的最小能力子集。
type wireClient interface {
	// checkSession 校验当前 cookie 会话是否有效。
	checkSession(ctx context.Context) error
	// rapidUpload 纯哈希秒传：按 sha1+文件名+大小秒传进 dirCID。
	rapidUpload(ctx context.Context, dirCID, fileName, sha1 string, size int64) (rapidOutcome, error)
	// findBySHA1 在给定目录（含下钻扫描，见实现）中按 SHA1 找文件，未找到返回 (nil, nil)。
	findBySHA1(ctx context.Context, dirCID, sha1 string) (*fileMeta, error)
	// downloadURL 按 pickCode 取一条新鲜直链（115 直链有时效）。
	downloadURL(ctx context.Context, pickCode string) (string, error)
	// listDir 按 cid 列出子目录（一页，NextFind /directories 语义）。
	listDir(ctx context.Context, cid string) ([]fileMeta, error)
	// mkdir 在 parentCID 下新建目录，返回新目录 cid。
	mkdir(ctx context.Context, parentCID, name string) (string, error)
	// dirCIDByPath 按路径（如 "/最近接收"）解析 cid。
	dirCIDByPath(ctx context.Context, path string) (string, error)
}

// 115 浏览器 UA（对标 alist 115 驱动的做法）。
const ua115Browser = "Mozilla/5.0 115Browser/27.0.5.7"

// realWire 是 wireClient 的真实实现，基于 115driver。
type realWire struct {
	client *driver115.Pan115Client
}

func newRealWire(cookie string) (*realWire, error) {
	cr := &driver115.Credential{}
	if err := cr.FromCookie(cookie); err != nil {
		// cookie 格式错误：fail-fast，不把 cookie 内容放进错误。
		return nil, &AuthError{Op: "parse_cookie", Err: err}
	}
	c := driver115.New(driver115.UA(ua115Browser))
	c.ImportCredential(cr)
	return &realWire{client: c}, nil
}

func (w *realWire) checkSession(ctx context.Context) error {
	if err := w.client.LoginCheck(); err != nil {
		return &AuthError{Op: "login_check", Err: scrubErr(err)}
	}
	return nil
}

// emptySeeker 是一个空的 io.ReadSeeker：纯哈希秒传没有文件字节，
// 仅在 115 触发 range 签名挑战（status 7）时会被读取，此时证明必然失败，
// 调用方会把结果判为 rapidSignCheck。bytes.Reader 的 Seek/Read 不会 panic。
func emptySeeker() *bytes.Reader { return bytes.NewReader(nil) }

func (w *realWire) rapidUpload(ctx context.Context, dirCID, fileName, sha1 string, size int64) (rapidOutcome, error) {
	// 115 要求 SHA1 大写。
	id := strings.ToUpper(sha1)
	// preID（前 128KB 的 sha1）参与 token 签名；纯哈希秒传没有文件字节，
	// 传空字符串，服务端是否接受待真实联调验证（见 README 风险点）。
	resp, err := w.client.RapidUpload(size, fileName, dirCID, "", id, emptySeeker())
	if err != nil {
		if e := asAuthError(err); e != nil {
			return rapidMiss, e
		}
		return rapidMiss, scrubErr(err)
	}
	ok, err := resp.Ok()
	if err != nil {
		// 非 1/2 的未知状态：可能是触发了签名挑战后失败，
		// 也可能是协议变更。保守判为 rapidSignCheck 供上层记录，
		// 由调用方统一转成 RapidMissError。
		if isSignCheckStatus(resp) {
			return rapidSignCheck, nil
		}
		if e := asAuthError(err); e != nil {
			return rapidMiss, e
		}
		return rapidMiss, fmt.Errorf("adapter115: unexpected rapid upload status %d: %w", resp.Status, scrubErr(err))
	}
	if ok {
		return rapidHit, nil
	}
	return rapidMiss, nil
}

// isSignCheckStatus 尽力判断是否为签名挑战相关状态。
// 115driver 内部已处理 status 7 的循环；能观察到的残留状态多为 7 或其他非 1/2 值。
func isSignCheckStatus(resp *driver115.UploadInitResp) bool {
	return resp != nil && resp.Status == 7
}

func (w *realWire) findBySHA1(ctx context.Context, dirCID, sha1 string) (*fileMeta, error) {
	// 115 没有"按 sha1 全局查询"的公开接口：按目录分页列出并比对 Sha1 字段。
	// 先查接收目录（秒传落盘位置，命中率最高），再查根目录做"全域"兜底。
	if m, err := w.scanDirForSHA1(dirCID, sha1); err != nil || m != nil {
		return m, scrubAuthErr(err)
	}
	if dirCID != "0" {
		if m, err := w.scanDirForSHA1("0", sha1); err != nil || m != nil {
			return m, scrubAuthErr(err)
		}
	}
	return nil, nil
}

// maxScanPages 限制单次探测扫描的页数，避免大目录拖慢播放链路。
const maxScanPages = 3

// dirPageLimit 与 115driver 的 MaxDirPageLimit 对齐。
const dirPageLimit = 1150

func (w *realWire) scanDirForSHA1(dirCID, sha1 string) (*fileMeta, error) {
	for page := int64(0); page < maxScanPages; page++ {
		files, err := w.client.ListPage(dirCID, page*dirPageLimit, dirPageLimit)
		if err != nil {
			return nil, err
		}
		if files == nil || len(*files) == 0 {
			return nil, nil
		}
		for _, f := range *files {
			if f.IsDirectory {
				continue
			}
			if strings.EqualFold(f.Sha1, sha1) {
				return &fileMeta{
					FileID: f.FileID, Name: f.Name, Size: f.Size,
					SHA1: f.Sha1, PickCode: f.PickCode, CID: dirCID,
				}, nil
			}
		}
		if int64(len(*files)) < dirPageLimit {
			return nil, nil // 最后一页
		}
	}
	return nil, nil // 扫描上限内未找到
}

func (w *realWire) downloadURL(ctx context.Context, pickCode string) (string, error) {
	info, err := w.client.Download(pickCode)
	if err != nil {
		return "", err
	}
	if info == nil || !info.Url.Valid || info.Url.Url == "" {
		return "", driver115.ErrDownloadEmpty
	}
	return info.Url.Url, nil
}

func (w *realWire) listDir(ctx context.Context, cid string) ([]fileMeta, error) {
	files, err := w.client.ListPage(cid, 0, dirPageLimit)
	if err != nil {
		return nil, err
	}
	out := make([]fileMeta, 0, len(*files))
	for _, f := range *files {
		out = append(out, fileMeta{
			FileID: f.FileID, Name: f.Name, Size: f.Size,
			SHA1: f.Sha1, PickCode: f.PickCode, CID: cid, IsDir: f.IsDirectory,
		})
	}
	return out, nil
}

func (w *realWire) mkdir(ctx context.Context, parentCID, name string) (string, error) {
	return w.client.Mkdir(parentCID, name)
}

func (w *realWire) dirCIDByPath(ctx context.Context, path string) (string, error) {
	resp, err := w.client.DirName2CID(path)
	if err != nil {
		return "", err
	}
	return string(resp.CategoryID), nil
}

// ---------------------------------------------------------------- 错误分类

// scrubErr 把底层错误里的敏感信息去掉（防御性：cookie 不应出现在错误里，
// 但以防万一做一次过滤）。
func scrubErr(err error) error {
	if err == nil {
		return nil
	}
	// 115driver 的错误不含 cookie，这里只做透传；保留包装以便 errors.Is/As。
	return err
}

// scrubAuthErr 把认证类错误转成 *AuthError，其余透传。
func scrubAuthErr(err error) error {
	if e := asAuthError(err); e != nil {
		return e
	}
	return err
}

// asAuthError 识别 115driver 的认证失败（sentinel + 关键词启发式）。
func asAuthError(err error) *AuthError {
	if err == nil {
		return nil
	}
	for _, s := range []error{
		driver115.ErrBadCookie,
		driver115.ErrNotLogin,
		driver115.ErrFailedToLogin,
		driver115.ErrDoesLoggedOut,
	} {
		if errors.Is(err, s) {
			return &AuthError{Op: "115api", Err: err}
		}
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{
		"not login", "bad cookie", "cookie expired", "cookie invalid",
		"unauthorized", "login expired", "session expired",
		"needs login", "please login", "kicked out",
		"未登录", "登录过期", "登录失效", "cookie失效", "cookie过期",
	} {
		if strings.Contains(msg, kw) {
			return &AuthError{Op: "115api", Err: err}
		}
	}
	return nil
}

// isRetryableNetErr 判断是否为值得重试的网络/服务端瞬时错误。
func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return isRetryableNetErr(uerr.Err)
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{
		"connection reset", "connection refused", "broken pipe",
		"timeout", "temporarily", "try again", "rate limit",
		"too many requests", "service unavailable",
		" 502", " 503", " 504", "code 502", "code 503", "code 504",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// ensure wireClient 被 realWire 完整实现（编译期检查）。
var _ wireClient = (*realWire)(nil)
