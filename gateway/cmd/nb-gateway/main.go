package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	a115 "nextemby-replay/adapter115"
	"nextemby-replay/engine"
	emem "nextemby-replay/engine/memory"
	"nextemby-replay/gateway"
	"nextemby-replay/store"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildEngine 用最终确定的 drives 表 + 持久化存储组装引擎。
// 演示用户 lzy（池模式/vip）与 guest 由 store.EnsureDefaults 种子写入。
func buildEngine(drives map[string]engine.DriveClient, seedID string,
	users engine.UserStore, records engine.PlaybackRecordStore, pool engine.PoolStore,
	templateOf func(string) (engine.Template, bool)) *engine.Engine {
	poolDrv := map[string]engine.DriveClient{}
	for id, d := range drives {
		if id == seedID {
			continue
		}
		poolDrv[id] = d
	}
	return engine.New(
		engine.Config{
			Templates:    map[string]int{"vip": 5},
			DefaultLimit: 1,
			TemplateOf:   templateOf, // 优先读 DB templates 表（60s 缓存）
		},
		engine.Deps{
			Users:   users,
			Pool:    pool,
			Seed:    drives[seedID],
			PoolDrv: poolDrv,
			Shield:  emem.NewShieldClient(map[string]engine.ShieldResult{}),
			Records: records,
		},
	)
}

// applyDBNetworkConfig：settings 的 emby.addr / emby.port / emby.proxy_port
// 非空时覆盖启动配置（重启生效；scheme 沿用 NB_UPSTREAM 的）。
func applyDBNetworkConfig(ctx context.Context, st *store.Store, cfg *gateway.Config, logger *log.Logger) {
	if addr, ok, _ := st.GetSetting(ctx, "emby.addr"); ok && addr != "" {
		scheme := "http"
		if strings.HasPrefix(cfg.Upstream, "https://") {
			scheme = "https"
		}
		host := addr
		if port, ok, _ := st.GetSetting(ctx, "emby.port"); ok && port != "" {
			host = addr + ":" + port
		}
		cfg.Upstream = scheme + "://" + host
		logger.Printf("emby 上游地址来自 DB 配置（重启生效）: %s", cfg.Upstream)
	}
	if port, ok, _ := st.GetSetting(ctx, "emby.proxy_port"); ok && port != "" {
		cfg.Listen = ":" + port
		logger.Printf("监听地址来自 DB 配置（重启生效）: %s", cfg.Listen)
	}
}

// seedEnvCookieToDB 把环境变量里的 115 Cookie 种子进 DB：
// 仅当 DB 里该账号尚无 cookie 时写入；成功后打日志（不含 cookie 值），之后以 DB 为准。
func seedEnvCookieToDB(ctx context.Context, as *store.Accounts115Store, logger *log.Logger,
	envKey, accountID, kind string) {
	cookie := os.Getenv(envKey)
	if cookie == "" {
		return
	}
	_, ok, err := as.AccountCookie(ctx, accountID)
	if err != nil {
		logger.Printf("检查115账号 cookie 失败 id=%s: %v", accountID, err)
		return
	}
	if ok {
		return // DB 已有，以 DB 为准
	}
	if err := as.UpsertAccount115(ctx, store.Account115Input{
		ID: accountID, Name: accountID, Kind: kind, Cookie: cookie,
		Healthy: true, Enabled: true,
	}); err != nil {
		logger.Printf("115 Cookie 种子进 DB 失败 id=%s: %v", accountID, err)
		return
	}
	logger.Printf("环境变量 %s 的 Cookie 已种子进 DB（账号 %s），之后以 DB 为准", envKey, accountID)
}

// templateOfFunc 返回读 DB templates 表的 TemplateOf（60s 缓存）；
// 未命中返回 (Template{}, false)，引擎回退 DefaultLimit。
func templateOfFunc(ctx context.Context, st *store.Store) func(string) (engine.Template, bool) {
	type entry struct {
		t   engine.Template
		ok  bool
		exp time.Time
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return func(name string) (engine.Template, bool) {
		mu.Lock()
		e, hit := cache[name]
		mu.Unlock()
		if hit && time.Now().Before(e.exp) {
			return e.t, e.ok
		}
		t, found, err := st.GetTemplate(ctx, name)
		var et engine.Template
		ok := false
		if err == nil && found {
			et = engine.Template{Name: t.Name, MaxConcurrent: t.MaxConcurrent, MaxDevices: t.MaxDevices}
			ok = true
		}
		mu.Lock()
		cache[name] = entry{t: et, ok: ok, exp: time.Now().Add(60 * time.Second)}
		mu.Unlock()
		return et, ok
	}
}

func main() {
	logger := log.New(os.Stdout, "[gateway] ", log.LstdFlags)

	cfg, err := gateway.LoadConfig()
	if err != nil {
		logger.Fatalf("配置错误: %v", err)
	}

	// SQLite 持久化：用户 / Cookie / 播放记录 / 决策 / 池账号 / 配置 / 115 账号 /
	// 路径映射 / 模板 / 系统日志。
	// 路径经 NB_DB_PATH 配置（默认 ./nextemby.db；docker 里用 /data/nextemby.db）。
	ctx := context.Background()
	st, err := store.Open(envOr("NB_DB_PATH", "./nextemby.db"))
	if err != nil {
		logger.Fatalf("数据库打开失败: %v", err)
	}
	defer st.Close()
	if err := st.EnsureDefaults(ctx); err != nil {
		logger.Fatalf("数据库种子数据失败: %v", err)
	}

	// 管理后台可配的上游/监听地址覆盖（重启生效）。
	applyDBNetworkConfig(ctx, st, &cfg, logger)

	// 115 账号存储（Cookie AES-GCM 加解密，密钥 NB_COOKIE_KEY；cookie 明文永不打日志）。
	a115store, err := st.Accounts115(cfg.CookieKey)
	if err != nil {
		logger.Fatalf("115 账号存储初始化失败: %v", err)
	}

	// 环境变量 Cookie 种子进 DB（仅 DB 无 cookie 时；种子号与池号各一）。
	// Cookie 只从环境变量读一次，之后以 DB 为准；cookie 值绝不出现在日志里。
	seedEnvCookieToDB(ctx, a115store, logger, "NB_115_SEED_COOKIE", envOr("NB_115_SEED_ACCOUNT", "115大"), "seed")
	seedEnvCookieToDB(ctx, a115store, logger, "NB_115_COOKIE", envOr("NB_115_ACCOUNT", "115小1"), "pool")

	// drives 从 accounts115 建：有 cookie 的做 115 LoginCheck 建真实 client，
	// 登录失败不 fatal（记 error 日志，用内存 demo client 占位）；无 cookie 用 demo client。
	accs, err := a115store.ListAccounts115(ctx)
	if err != nil {
		logger.Fatalf("115 账号列表失败: %v", err)
	}
	seedID := envOr("NB_115_SEED_ACCOUNT", "115大")
	for _, a := range accs {
		if a.Kind == "seed" {
			seedID = a.ID // kind=seed 的第一行（ListAccounts115 按 kind,name 排序）
			break
		}
	}
	drives := map[string]engine.DriveClient{}
	for _, a := range accs {
		ck, ok, err := a115store.AccountCookie(ctx, a.ID)
		if err != nil {
			logger.Printf("读115账号 cookie 失败 id=%s: %v", a.ID, err)
		}
		if ok && ck != "" {
			c, err := a115.New(ctx, a.ID, ck)
			if err != nil {
				logger.Printf("115 账号 %s 登录失败，用演示 client 占位: %v", a.ID, err)
				drives[a.ID] = emem.NewDriveClient(a.ID)
			} else {
				drives[a.ID] = c
				logger.Printf("115 账号已接入: %s（kind=%s）", a.ID, a.Kind)
			}
		} else {
			drives[a.ID] = emem.NewDriveClient(a.ID)
		}
	}
	if _, ok := drives[seedID]; !ok {
		drives[seedID] = emem.NewDriveClient(seedID)
	}

	// 指纹解析的 fetcher：种子账号（115大 持有全量库）的真实 115 client；
	// 种子号未接入真实 client 时为 nil，指纹恒 miss，优雅降级为原样透传。
	var fetcher gateway.SHA1Fetcher
	if c, ok := drives[seedID].(*a115.Client); ok {
		fetcher = c
	}

	// 池账号入库（幂等）：accounts115 里 kind=pool 的账号全部 EnsureAccount。
	poolStore := st.Pool()
	for _, a := range accs {
		if a.Kind != "pool" {
			continue
		}
		maxUsers := a.MaxUsers
		if maxUsers <= 0 {
			maxUsers = 4
		}
		if err := poolStore.EnsureAccount(ctx, a.ID, maxUsers); err != nil {
			logger.Fatalf("池账号入库失败: %v", err)
		}
	}

	eng := buildEngine(drives, seedID, st.Users(), st.Records(), poolStore, templateOfFunc(ctx, st))
	// 决策持久化：每次 Handle 结束同步写 decisions 表（失败只记日志，不影响播放）。
	eng.OnDecision = func(sum engine.DecisionSummary) {
		if err := st.LogDecision(sum); err != nil {
			logger.Printf("决策持久化失败: %v", err)
		}
	}

	// 指纹解析：Emby路径 → 路径映射 → 115路径 → 播放时实时取 sha1，不预扫。
	// 规则经 srv.ReloadPathMaps() 从 DB（优先）或 NB_PATH_MAP 加载，可热更新。
	finger := gateway.NewLiveFingerprintResolver(nil, fetcher)

	srv, err := gateway.NewServer(cfg, eng, drives, finger, logger)
	if err != nil {
		logger.Fatalf("启动失败: %v", err)
	}

	// Web UI：管理后台（/nb/admin，密码登录/session，兼容 NB_ADMIN_TOKEN）+
	// 个人中心（/nb/me）。
	// Cookie 校验走 115 LoginCheck（a115.New 构造时 fail-fast）；Cookie 存 SQLite（AES-GCM）。
	cookies, err := st.Cookies(cfg.CookieKey)
	if err != nil {
		logger.Fatalf("Cookie 存储初始化失败: %v", err)
	}
	srv.EnableWebUI(cfg.AdminToken, cookies,
		func(ctx context.Context, userID, cookie string) (engine.DriveClient, error) {
			return a115.New(ctx, userID, cookie)
		})
	srv.AttachStore(st)
	srv.AttachLogSink(st) // engine 中文播放日志 + 网关事件异步落库
	srv.ReloadPathMaps()  // DB path_maps（优先）/ NB_PATH_MAP → 指纹解析热更新
	logger.Printf("listening on %s, upstream=%s", cfg.Listen, cfg.Upstream)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.Handler()))
}
