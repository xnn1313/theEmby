// Command livetest — 三模块真实联调入口（需要用户提供 115 测试号 Cookie）。
//
// 用法（环境变量，密钥只进内存、绝不打印）：
//
//	NB_API_KEY     上游 Emby 服务 Key（必填）
//	NB_115_COOKIE  115 测试号 Cookie（必填）
//	NB_UPSTREAM    上游 Emby 地址（默认 https://embyf.bbstzb.org）
//	NB_115_ACCOUNT 115 账号标识（默认 "115小测"）
//	NB_PATH_MAP    路径映射（默认 "/CloudNAS/CloudDrive/115open=/"，nextemby 形态）
//	NB_LISTEN      网关监听地址（默认 ":8091"）
//	NB_HMAC_KEY    /nb/stream 签发密钥（可选）
//	NB_PROBE_PATH  可选：一条真实 Emby 路径，启动前做一次指纹自检
//	               （路径映射 → 115 实时取 sha1 → 全域探测），直接回答
//	               adapter115 README 里"目录扫描命中率"的待验证项。
//
// 启动后：网关对外服务，PlaybackInfo 走真实决策链（探测→神盾miss→秒传→直链）。
// 用户/池/神盾/播放记录暂用内存实现——联调 115 链路不需要它们落库。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"nextemby-replay/adapter115"
	"nextemby-replay/engine"
	"nextemby-replay/engine/memory"
	"nextemby-replay/gateway"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func required(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("livetest: 缺少必填环境变量 %s", key)
	}
	return v
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ctx := context.Background()

	// 密钥只读一次进局部变量，不打印、不落盘。
	apiKey := required("NB_API_KEY")
	cookie := required("NB_115_COOKIE")
	upstream := getenv("NB_UPSTREAM", "https://embyf.bbstzb.org")
	account := getenv("NB_115_ACCOUNT", "115小测")
	pathMap := getenv("NB_PATH_MAP", "/CloudNAS/CloudDrive/115open=/")
	listen := getenv("NB_LISTEN", ":8091")
	probePath := os.Getenv("NB_PROBE_PATH")
	_ = apiKey // 下面组装 gateway.Config 时使用

	// 1. 真实 115 客户端 + Cookie 有效性校验。
	drv, err := adapter115.New(ctx, account, cookie,
		adapter115.WithLogger(log.New(os.Stderr, "[115] ", log.LstdFlags)))
	if err != nil {
		log.Fatalf("livetest: 创建 115 客户端失败: %v", err)
	}
	if err := drv.CheckSession(ctx); err != nil {
		log.Fatalf("livetest: 115 Cookie 无效或已过期: %v", err)
	}
	log.Printf("livetest: 115 会话有效，账号=%s", drv.AccountID())

	// 2. 可选：真实指纹自检（路径映射 → 实时取 sha1 → 全域探测）。
	if probePath != "" {
		rules, err := gateway.ParsePathMap(pathMap)
		if err != nil {
			log.Fatalf("livetest: 路径映射解析失败: %v", err)
		}
		resolver := gateway.NewLiveFingerprintResolver(gateway.NewPathMapper(rules), drv)
		t0 := time.Now()
		sha1, ok := resolver.ResolveSHA1(ctx, probePath)
		if !ok {
			log.Fatalf("livetest: 指纹自检失败：路径 %q 映射后在 115 上找不到文件", probePath)
		}
		hit, err := drv.ProbeSHA1(ctx, sha1)
		if err != nil {
			log.Fatalf("livetest: 全域探测失败: %v", err)
		}
		log.Printf("livetest: 指纹自检 OK sha1=%.12s… 探测=%v 耗时=%v", sha1, hit, time.Since(t0).Round(time.Millisecond))
	}

	// 3. 引擎（内存用户/池/神盾/记录）+ 真实 115 盘。
	eng := engine.New(
		engine.Config{Templates: map[string]int{"vip": 5}},
		engine.Deps{
			Users:   memory.NewUserStore(engine.User{ID: "livetest", Mode: engine.ModePool, Template: "vip"}),
			Pool:    memory.NewPoolStore(engine.PoolAccount{ID: account, Healthy: true, MaxUsers: 4}),
			Seed:    drv, // 联调阶段种子与服务同号；生产再拆分 115大/115小N
			PoolDrv: map[string]engine.DriveClient{account: drv},
			Shield:  memory.NewShieldClient(nil),
			Records: memory.NewPlaybackRecordStore(),
		},
	)

	// 4. 网关：透传上游 + 劫持 PlaybackInfo。
	rules, err := gateway.ParsePathMap(pathMap)
	if err != nil {
		log.Fatalf("livetest: 路径映射解析失败: %v", err)
	}
	finger := gateway.NewLiveFingerprintResolver(gateway.NewPathMapper(rules), drv)
	srv, err := gateway.NewServer(
		gateway.Config{
			Upstream: upstream,
			APIKey:   apiKey,
			Listen:   listen,
			HMACKey:  []byte(getenv("NB_HMAC_KEY", "")),
			PathMap:  pathMap,
			CertFile: os.Getenv("SSL_CERT_FILE"),
		},
		eng,
		map[string]engine.DriveClient{account: drv},
		finger,
		log.Default(),
	)
	if err != nil {
		log.Fatalf("livetest: 网关组装失败: %v", err)
	}

	fmt.Printf(`livetest 就绪:
  上游: %s
  115 账号: %s（真实）
  路径映射: %s
  监听: %s

验证步骤:
  1. Emby 客户端把服务器地址指向本网关（X-NB-User: livetest）
  2. 播放 /CloudNAS/CloudDrive/115open 下的任意影片
  3. 观察日志：应出现 playbackinfo branch=pool_* account=%s
  4. 客户端应秒开（秒传 ~1.5s + 直链 ~0.3s），拖进度条走 10min 缓存
`, upstream, account, pathMap, listen, account)
	log.Fatal(http.ListenAndServe(listen, srv.Handler()))
}
