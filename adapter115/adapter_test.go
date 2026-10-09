package adapter115

// 本文件用 fakeWire（wireClient 的内存假实现）覆盖适配器的全部分支，
// 不需要真实 115 账号、不产生任何网络 I/O。
//
// 说明：115 的秒传 / 取直链是加密协议（ECDH / m115），用 httptest 在 HTTP 层
// 复刻 mock 服务端需要重实现 115 的加密握手——这正是我们依赖 115driver 的原因。
// 因此测试收敛在 wireClient 缝隙上：fake 注入任意分支，覆盖全部业务决策逻辑；
// 线路协议本身由 115driver（alist 115 驱动同款实现）保证。

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSHA1  = "A2375D8E9C1B2A3D4E5F60718293A4B5C6D7E8F90"
	testSHA1B = "6CB19DAB2C3D4E5F60718293A4B5C6D7E8F90123"
	testName  = "去有风的地方.Meet Yourself.2023.S01E04.mp4"
	testSize  = int64(5497558016) // 5.12GB
	testCID   = "cid_recv_001"
	testURL   = "https://cdn.115.com/mock/abc123?token=x"
)

var errFakeAuth = errors.New("fake: user not login")

type timeoutError struct{}

func (timeoutError) Error() string   { return "fake: dial timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// fakeWire 是 wireClient 的可编排假实现。
type fakeWire struct {
	mu         sync.Mutex
	sessionErr error

	rapidOut rapidOutcome
	rapidErr error

	// files: dirCID -> UPPER(sha1) -> meta
	files map[string]map[string]*fileMeta

	dlURL string
	dlErr error

	mkdirCID string
	mkdirErr error

	dirCIDResult string
	dirCIDErr    error
	dirCIDEmpty  bool // 模拟目录不存在（返回空 cid，走自动创建分支）

	listData map[string][]fileMeta
	listErr  error

	calls []string
}

func (f *fakeWire) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeWire) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeWire) checkSession(ctx context.Context) error {
	f.record("checkSession")
	return f.sessionErr
}

func (f *fakeWire) rapidUpload(ctx context.Context, dirCID, fileName, sha1 string, size int64) (rapidOutcome, error) {
	f.record("rapidUpload:dir=" + dirCID)
	if f.rapidErr != nil {
		return rapidMiss, f.rapidErr
	}
	return f.rapidOut, nil
}

func (f *fakeWire) findBySHA1(ctx context.Context, dirCID, sha1 string) (*fileMeta, error) {
	f.record("findBySHA1:" + dirCID)
	if m, ok := f.files[dirCID][strings.ToUpper(sha1)]; ok {
		cp := *m
		return &cp, nil
	}
	if m, ok := f.files["0"][strings.ToUpper(sha1)]; ok {
		cp := *m
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeWire) downloadURL(ctx context.Context, pickCode string) (string, error) {
	f.record("downloadURL:" + pickCode)
	if f.dlErr != nil {
		return "", f.dlErr
	}
	return f.dlURL, nil
}

func (f *fakeWire) listDir(ctx context.Context, cid string) ([]fileMeta, error) {
	f.record("listDir:" + cid)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listData[cid], nil
}

func (f *fakeWire) mkdir(ctx context.Context, parentCID, name string) (string, error) {
	f.record("mkdir:parent=" + parentCID + ":name=" + name)
	if f.mkdirErr != nil {
		return "", f.mkdirErr
	}
	return f.mkdirCID, nil
}

func (f *fakeWire) dirCIDByPath(ctx context.Context, path string) (string, error) {
	f.record("dirCIDByPath:" + path)
	if f.dirCIDErr != nil {
		return "", f.dirCIDErr
	}
	if f.dirCIDEmpty {
		return "", nil
	}
	return f.dirCIDResult, nil
}

// authFailWire 在除 dirCIDByPath 外的所有操作上固定返回认证错误。
type authFailWire struct{ err error }

func (w *authFailWire) checkSession(ctx context.Context) error { return nil }
func (w *authFailWire) rapidUpload(ctx context.Context, dirCID, fileName, sha1 string, size int64) (rapidOutcome, error) {
	return rapidMiss, w.err
}
func (w *authFailWire) findBySHA1(ctx context.Context, dirCID, sha1 string) (*fileMeta, error) {
	return nil, w.err
}
func (w *authFailWire) downloadURL(ctx context.Context, pickCode string) (string, error) {
	return "", w.err
}
func (w *authFailWire) listDir(ctx context.Context, cid string) ([]fileMeta, error) {
	return nil, w.err
}
func (w *authFailWire) mkdir(ctx context.Context, parentCID, name string) (string, error) {
	return "", w.err
}
func (w *authFailWire) dirCIDByPath(ctx context.Context, path string) (string, error) {
	return "cid", nil
}

// testClient 构造带日志采集的测试客户端。
func testClient(f *fakeWire, buf *bytes.Buffer, opts ...Option) *Client {
	base := []Option{WithLogger(log.New(buf, "", 0))}
	return newForTest("115小2", f, append(base, opts...)...)
}

func ctx() context.Context { return context.Background() }

func sizeResolver(_ context.Context, sha1 string) (int64, bool) {
	if strings.EqualFold(sha1, testSHA1) {
		return testSize, true
	}
	return 0, false
}

// ------------------------------------------------------------ 构造与认证

func TestNew_BadCookieFormat(t *testing.T) {
	// FromCookie 要求至少 3 个 ;-分隔的键值对；这里不产生任何网络 I/O。
	_, err := New(ctx(), "115小2", "UID=SECRET_COOKIE_VALUE")
	if err == nil {
		t.Fatal("expected error for malformed cookie")
	}
	if !IsAuthError(err) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
	if strings.Contains(err.Error(), "SECRET_COOKIE_VALUE") {
		t.Fatal("error must not contain cookie content")
	}
}

func TestCheckSession_AuthFailFast(t *testing.T) {
	f := &fakeWire{sessionErr: errFakeAuth}
	c := newClient("115小2", f)
	err := c.CheckSession(ctx())
	if !IsAuthError(err) {
		t.Fatalf("expected *AuthError, got %v", err)
	}
	var ae *AuthError
	if errors.As(err, &ae); ae.AccountID != "115小2" {
		t.Fatalf("AccountID not carried: %+v", ae)
	}
}

func TestCheckSession_OK(t *testing.T) {
	f := &fakeWire{}
	c := newForTest("115小2", f)
	if err := c.CheckSession(ctx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ------------------------------------------------------------ 全域 SHA1 探测

func TestProbeSHA1_Hit(t *testing.T) {
	f := &fakeWire{
		dirCIDResult: testCID,
		files: map[string]map[string]*fileMeta{
			testCID: {testSHA1: {Name: testName, SHA1: testSHA1, PickCode: "pc1"}},
		},
	}
	var buf bytes.Buffer
	c := testClient(f, &buf)
	hit, err := c.ProbeSHA1(ctx(), testSHA1)
	if err != nil || !hit {
		t.Fatalf("hit=%v err=%v", hit, err)
	}
	if !strings.Contains(buf.String(), "命中") {
		t.Fatalf("log should mark hit branch, got: %s", buf.String())
	}
}

func TestProbeSHA1_Miss(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, files: map[string]map[string]*fileMeta{}}
	var buf bytes.Buffer
	c := testClient(f, &buf)
	hit, err := c.ProbeSHA1(ctx(), strings.ToLower(testSHA1)) // 小写也要能比对
	if err != nil || hit {
		t.Fatalf("hit=%v err=%v", hit, err)
	}
	if !strings.Contains(buf.String(), "未命中") {
		t.Fatalf("log should mark miss branch, got: %s", buf.String())
	}
}

func TestProbeSHA1_RootFallback(t *testing.T) {
	// 接收目录没有，根目录有 -> 全域兜底命中
	f := &fakeWire{
		dirCIDResult: testCID,
		files: map[string]map[string]*fileMeta{
			"0": {testSHA1: {Name: testName, SHA1: testSHA1, PickCode: "pc9"}},
		},
	}
	c := newForTest("115小2", f)
	hit, err := c.ProbeSHA1(ctx(), testSHA1)
	if err != nil || !hit {
		t.Fatalf("hit=%v err=%v", hit, err)
	}
}

func TestProbeSHA1_AuthError(t *testing.T) {
	c := newForTest("115小2", &authFailWire{err: errFakeAuth})
	_, err := c.ProbeSHA1(ctx(), testSHA1)
	if !IsAuthError(err) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

// ------------------------------------------------------------ 秒传

func TestRapidTransfer_Hit(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, rapidOut: rapidHit}
	var buf bytes.Buffer
	c := testClient(f, &buf, WithSizeResolver(sizeResolver))
	dir, err := c.RapidTransfer(ctx(), testSHA1, testName, 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "/最近接收" {
		t.Fatalf("dir=%q", dir)
	}
	out := buf.String()
	if !strings.Contains(out, "秒传成功") || !strings.Contains(out, "耗时:") {
		t.Fatalf("log should mark success branch with elapsed, got: %s", out)
	}
}

func TestRapidTransfer_MissNotInPool(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, rapidOut: rapidMiss}
	c := newForTest("115小2", f, WithSizeResolver(sizeResolver))
	_, err := c.RapidTransfer(ctx(), testSHA1, testName, 1<<20)
	if !IsRapidMiss(err) {
		t.Fatalf("expected *RapidMissError, got %T: %v", err, err)
	}
	var rm *RapidMissError
	if errors.As(err, &rm); rm.Reason != RapidMissNotInPool {
		t.Fatalf("reason=%s", rm.Reason)
	}
}

func TestRapidTransfer_SignCheck(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, rapidOut: rapidSignCheck}
	c := newForTest("115小2", f, WithSizeResolver(sizeResolver))
	_, err := c.RapidTransfer(ctx(), testSHA1, testName, 1<<20)
	var rm *RapidMissError
	if errors.As(err, &rm); rm.Reason != RapidMissSignCheck {
		t.Fatalf("expected sign_check_required, got %v", err)
	}
}

func TestRapidTransfer_NeedFileSize(t *testing.T) {
	// 没有 SizeResolver 且 fileSize<=0 -> 无法调用 115 秒传接口，返回明确的 typed error，
	// 且不能碰线路层。
	f := &fakeWire{dirCIDResult: testCID, rapidOut: rapidHit}
	c := newForTest("115小2", f)
	_, err := c.RapidTransfer(ctx(), testSHA1, testName, 0)
	var rm *RapidMissError
	if errors.As(err, &rm); rm.Reason != RapidMissNeedFileSize {
		t.Fatalf("expected need_file_size, got %v", err)
	}
	if f.callCount("rapidUpload") != 0 {
		t.Fatal("must not call wire without file size")
	}
}

func TestRapidTransfer_AuthError(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, rapidErr: errFakeAuth}
	c := newForTest("115小2", f, WithSizeResolver(sizeResolver))
	_, err := c.RapidTransfer(ctx(), testSHA1, testName, 1<<20)
	if !IsAuthError(err) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

// ------------------------------------------------------------ 取直链

func TestDirectURL_OK(t *testing.T) {
	f := &fakeWire{
		dirCIDResult: testCID,
		files: map[string]map[string]*fileMeta{
			testCID: {testSHA1: {Name: testName, SHA1: testSHA1, PickCode: "pc1"}},
		},
		dlURL: testURL,
	}
	var buf bytes.Buffer
	c := testClient(f, &buf)
	u, err := c.DirectURL(ctx(), testSHA1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != testURL {
		t.Fatalf("url=%q", u)
	}
	if !strings.Contains(buf.String(), "直连成功") {
		t.Fatalf("log should mark success, got: %s", buf.String())
	}
}

func TestDirectURL_FileNotFound(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, files: map[string]map[string]*fileMeta{}}
	c := newForTest("115小2", f)
	_, err := c.DirectURL(ctx(), testSHA1)
	if !IsFileNotFound(err) {
		t.Fatalf("expected *FileNotFoundError, got %T: %v", err, err)
	}
	if IsRetryable(err) {
		t.Fatal("file-not-found must not be retryable")
	}
}

func TestDirectURL_Retryable(t *testing.T) {
	f := &fakeWire{
		dirCIDResult: testCID,
		files: map[string]map[string]*fileMeta{
			testCID: {testSHA1: {Name: testSHA1, SHA1: testSHA1, PickCode: "pc1"}},
		},
		dlErr: &url.Error{Op: "Post", URL: "https://proapi.115.com/app/chrome/downurl", Err: timeoutError{}},
	}
	c := newForTest("115小2", f)
	_, err := c.DirectURL(ctx(), testSHA1)
	var du *DirectURLError
	if !errors.As(err, &du) {
		t.Fatalf("expected *DirectURLError, got %T: %v", err, err)
	}
	if !du.Retryable || !IsRetryable(err) {
		t.Fatalf("timeout should be retryable: %+v", du)
	}
}

func TestDirectURL_NotRetryable(t *testing.T) {
	f := &fakeWire{
		dirCIDResult: testCID,
		files: map[string]map[string]*fileMeta{
			testCID: {testSHA1: {Name: testSHA1, SHA1: testSHA1, PickCode: "pc1"}},
		},
		dlErr: errors.New("fake: pickcode does not exist"),
	}
	c := newForTest("115小2", f)
	_, err := c.DirectURL(ctx(), testSHA1)
	var du *DirectURLError
	if !errors.As(err, &du) {
		t.Fatalf("expected *DirectURLError, got %T: %v", err, err)
	}
	if du.Retryable || IsRetryable(err) {
		t.Fatalf("api error should not be retryable: %+v", du)
	}
}

func TestDirectURL_AuthError(t *testing.T) {
	c := newForTest("115小2", &authFailWire{err: errFakeAuth})
	_, err := c.DirectURL(ctx(), testSHA1)
	if !IsAuthError(err) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

// ------------------------------------------------------------ 目录能力

func TestMkdir(t *testing.T) {
	f := &fakeWire{mkdirCID: "cid_new_9"}
	c := newForTest("115小2", f)
	cid, err := c.Mkdir(ctx(), "0", "自动分类")
	if err != nil || cid != "cid_new_9" {
		t.Fatalf("cid=%q err=%v", cid, err)
	}
	if f.callCount("mkdir") != 1 {
		t.Fatalf("calls=%v", f.calls)
	}
}

func TestMkdir_DefaultRoot(t *testing.T) {
	f := &fakeWire{mkdirCID: "cid_new_9"}
	c := newForTest("115小2", f)
	if _, err := c.Mkdir(ctx(), "", "自动分类"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.callCount("mkdir:parent=0") != 1 {
		t.Fatalf("should default to root cid 0: %v", f.calls)
	}
}

func TestListDir(t *testing.T) {
	f := &fakeWire{
		listData: map[string][]fileMeta{
			"0": {
				{CID: "c1", Name: "最近接收", IsDir: true},
				{CID: "c2", Name: testName, Size: testSize, SHA1: testSHA1},
			},
		},
	}
	c := newForTest("115小2", f)
	entries, err := c.ListDir(ctx(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 || !entries[0].IsDir || entries[1].SHA1 != testSHA1 {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestFileSHA1ByPath(t *testing.T) {
	// 播放时实时取 sha1：/电影/去有风的地方 S01E04.mp4 -> dirCID(/电影) -> 列表命中。
	f := &fakeWire{
		dirCIDResult: "c_movie",
		listData: map[string][]fileMeta{
			"c_movie": {
				{CID: "f1", Name: "去有风的地方 S01E04.mp4", Size: testSize, SHA1: testSHA1},
				{CID: "d1", Name: "子目录", IsDir: true},
				{CID: "f2", Name: "无sha1.mp4", Size: testSize},
			},
		},
	}
	c := newForTest("115小2", f)
	sha, err := c.FileSHA1ByPath(ctx(), "/电影/去有风的地方 S01E04.mp4")
	if err != nil || sha != testSHA1 {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	// 文件名存在但 sha1 为空 -> 视为找不到
	if _, err := c.FileSHA1ByPath(ctx(), "/电影/无sha1.mp4"); err == nil {
		t.Fatal("expected error for entry without sha1")
	}
	// 不存在的文件 -> error（网关视为指纹 miss，走优雅降级）
	if _, err := c.FileSHA1ByPath(ctx(), "/电影/不存在.mp4"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRecvDirAutoCreate(t *testing.T) {
	// 目录不存在 -> recvCID 自动在根目录创建，且只创建一次（第二次走缓存）。
	// 用 ProbeSHA1 触发 recvCID 路径。
	f := &fakeWire{dirCIDEmpty: true, mkdirCID: "cid_auto_1", files: map[string]map[string]*fileMeta{}}
	c := newForTest("115小2", f)
	if _, err := c.ProbeSHA1(ctx(), testSHA1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := c.ProbeSHA1(ctx(), testSHA1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := f.callCount("mkdir"); n != 1 {
		t.Fatalf("mkdir should be called exactly once (cached), got %d: %v", n, f.calls)
	}
}

// ------------------------------------------------------------ 并发

func TestConcurrent(t *testing.T) {
	f := &fakeWire{
		dirCIDResult: testCID,
		rapidOut:     rapidHit,
		files: map[string]map[string]*fileMeta{
			testCID: {
				testSHA1:  {Name: testName, SHA1: testSHA1, PickCode: "pc1"},
				testSHA1B: {Name: "b.mkv", SHA1: testSHA1B, PickCode: "pc2"},
			},
		},
		dlURL: testURL,
	}
	c := newForTest("115小2", f, WithSizeResolver(func(_ context.Context, _ string) (int64, bool) {
		return testSize, true
	}))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sha := testSHA1
			if i%2 == 1 {
				sha = testSHA1B
			}
			if _, err := c.ProbeSHA1(ctx(), sha); err != nil {
				t.Errorf("probe: %v", err)
			}
			if _, err := c.RapidTransfer(ctx(), sha, testName, 1<<20); err != nil {
				t.Errorf("rapid: %v", err)
			}
			if _, err := c.DirectURL(ctx(), sha); err != nil {
				t.Errorf("direct: %v", err)
			}
			if _, err := c.ListDir(ctx(), "0"); err != nil {
				t.Errorf("list: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

// ------------------------------------------------------------ 安全：cookie 永不落日志

func TestCookieNeverLogged(t *testing.T) {
	// 坏 cookie 路径的错误里不能带 cookie
	_, err := New(ctx(), "115小2", "UID=SECRET_COOKIE_XYZ")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET_COOKIE_XYZ") {
		t.Fatalf("error leaks cookie: %v", err)
	}
	// Client 根本不存储 cookie 字符串；用完整调用链做回归断言。
	f := &fakeWire{
		dirCIDResult: testCID,
		rapidOut:     rapidHit,
		files: map[string]map[string]*fileMeta{
			testCID: {testSHA1: {Name: testSHA1, SHA1: testSHA1, PickCode: "pc1"}},
		},
		dlURL: testURL,
	}
	var buf bytes.Buffer
	c := testClient(f, &buf, WithSizeResolver(sizeResolver))
	_, _ = c.ProbeSHA1(ctx(), testSHA1)
	_, _ = c.RapidTransfer(ctx(), testSHA1, testName, 1<<20)
	_, _ = c.DirectURL(ctx(), testSHA1)
	if strings.Contains(buf.String(), "SECRET_COOKIE_XYZ") {
		t.Fatalf("log leaks cookie: %s", buf.String())
	}
}

// ------------------------------------------------------------ 错误语义

func TestErrorPredicates(t *testing.T) {
	if IsRapidMiss(nil) || IsAuthError(nil) || IsFileNotFound(nil) || IsRetryable(nil) {
		t.Fatal("predicates must be false for nil")
	}
	if IsRetryable(&RapidMissError{}) {
		t.Fatal("rapid miss must not be retryable")
	}
	if IsRetryable(&AuthError{}) {
		t.Fatal("auth error must not be retryable")
	}
	var rm *RapidMissError
	if !errors.As(wrapErr(&RapidMissError{Reason: RapidMissNotInPool}), &rm) {
		t.Fatal("RapidMissError should survive wrapping")
	}
}

func wrapErr(err error) error { return &wrapError{err} }

type wrapError struct{ err error }

func (w *wrapError) Error() string { return "wrap: " + w.err.Error() }
func (w *wrapError) Unwrap() error { return w.err }

func TestRapidMissError_Fields(t *testing.T) {
	f := &fakeWire{dirCIDResult: testCID, rapidOut: rapidMiss}
	c := newForTest("115小2", f, WithSizeResolver(sizeResolver))
	start := time.Now()
	_, err := c.RapidTransfer(ctx(), testSHA1, testName, 1<<20)
	var rm *RapidMissError
	if !errors.As(err, &rm) {
		t.Fatalf("expected *RapidMissError, got %T", err)
	}
	if rm.Elapsed < 0 || time.Since(start) < rm.Elapsed {
		t.Fatalf("elapsed not sane: %v", rm.Elapsed)
	}
	if rm.AccountID != "115小2" || rm.SHA1 != testSHA1 || rm.Reason != RapidMissNotInPool {
		t.Fatalf("fields not carried: %+v", rm)
	}
	if !strings.Contains(rm.Error(), "115小2") {
		t.Fatalf("Error() should carry account: %v", rm)
	}
}
