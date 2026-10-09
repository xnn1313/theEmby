package main

import (
	"context"
	"log"
	"net/http"
	"os"

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
	users engine.UserStore, records engine.PlaybackRecordStore, pool engine.PoolStore) *engine.Engine {
	poolDrv := map[string]engine.DriveClient{}
	for id, d := range drives {
		if id == seedID {
			continue
		}
		poolDrv[id] = d
	}
	return engine.New(
		engine.Config{Templates: map[string]int{"vip": 5}, DefaultLimit: 1},
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

func main() {
	logger := log.New(os.Stdout, "[gateway] ", log.LstdFlags)

	cfg, err := gateway.LoadConfig()
	if err != nil {
		logger.Fatalf("配置错误: %v", err)
	}

	// SQLite 持久化：用户 / Cookie / 播放记录 / 决策 / 池账号。
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

	// drives 先按内存演示装配；有 115 Cookie 则替换为真实 Client。
	// Cookie 只从环境变量读，构造时做 LoginCheck，失效直接 fail-fast，
	// 且绝不出现在日志里。
	seedID := envOr("NB_115_SEED_ACCOUNT", "115大")
	drives := map[string]engine.DriveClient{
		seedID:  emem.NewDriveClient(seedID),
		"115小1": emem.NewDriveClient("115小1"),
		"115小2": emem.NewDriveClient("115小2"),
	}

	var fetcher gateway.SHA1Fetcher
	if cookie := os.Getenv("NB_115_COOKIE"); cookie != "" {
		accountID := envOr("NB_115_ACCOUNT", "115小1")
		c, err := a115.New(context.Background(), accountID, cookie)
		if err != nil {
			logger.Fatalf("115 池账号登录失败: %v", err)
		}
		drives[accountID] = c
		fetcher = c
		logger.Printf("115 池账号已接入: %s（指纹实时取 + 秒传/直链走真实账号）", accountID)
	}
	if seedCookie := os.Getenv("NB_115_SEED_COOKIE"); seedCookie != "" {
		c, err := a115.New(context.Background(), seedID, seedCookie)
		if err != nil {
			logger.Fatalf("115 种子账号登录失败: %v", err)
		}
		drives[seedID] = c
		logger.Printf("115 种子账号已接入: %s", seedID)
	}

	// 池账号入库（幂等）：drives 里除种子外的账号全部 EnsureAccount。
	poolStore := st.Pool()
	for id := range drives {
		if id == seedID {
			continue
		}
		if err := poolStore.EnsureAccount(ctx, id, 4); err != nil {
			logger.Fatalf("池账号入库失败: %v", err)
		}
	}

	eng := buildEngine(drives, seedID, st.Users(), st.Records(), poolStore)
	// 决策持久化：每次 Handle 结束同步写 decisions 表（失败只记日志，不影响播放）。
	eng.OnDecision = func(sum engine.DecisionSummary) {
		if err := st.LogDecision(sum); err != nil {
			logger.Printf("决策持久化失败: %v", err)
		}
	}

	// 指纹解析：Emby路径 →（NB_PATH_MAP 映射）→ 115路径 → 播放时实时取 sha1，不预扫。
	// 无 fetcher 时恒 miss，优雅降级为原样透传。
	rules, err := gateway.ParsePathMap(cfg.PathMap)
	if err != nil {
		logger.Fatalf("NB_PATH_MAP 配置错误: %v", err)
	}
	finger := gateway.NewLiveFingerprintResolver(
		gateway.NewPathMapper(rules),
		fetcher,
	)

	srv, err := gateway.NewServer(cfg, eng, drives, finger, logger)
	if err != nil {
		logger.Fatalf("启动失败: %v", err)
	}

	// Web UI：管理后台（/nb/admin，NB_ADMIN_TOKEN 鉴权）+ 个人中心（/nb/me）。
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
	logger.Printf("listening on %s, upstream=%s", cfg.Listen, cfg.Upstream)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.Handler()))
}
