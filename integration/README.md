# integration — 三模块联调层

`~/workspace/nextemby-replay/integration/`：把模块 1（engine）、模块 2（gateway）、
模块 3（adapter115）接在一起验证。Mock 版零外部依赖；真实联调用 `cmd/livetest`。

## 已证明的（`go test -race ./...`，8 个测试全过）

| 测试 | 覆盖的真实链路 |
|---|---|
| `TestPoolProbeHitFullChain` | PlaybackInfo → 指纹解析 → 引擎决策 → 改写 `/nb/stream` → 302 到 115 直链（HTTP 全链路） |
| `TestEngineCacheAndLock` | 10min 缓存命中零 115 API + 24h 锁账号粘性 |
| `TestPoolShieldHitBranch` | 探测未命中 → 神盾命中 → 转存进池（`/最近接收`） |
| `TestPoolSeedFallbackBranch` | 神盾未命中 → 源盘秒传兜底 |
| `TestGracefulDegradation` | 路径映射 miss → 原样透传上游（不中断播放） |
| `TestConcurrencyDenied` | 同播超限 → 429 |
| `TestStreamTokenForged` | 伪造 token → 403 |
| `wirecheck.go` | 编译期断言：`adapter115.Client` 可直接插进 `engine.Deps` 与网关指纹解析器 |

结论：**真实 115 联调时只需把 memory 假实现换成 `adapter115.New` 的 Client，
引擎/网关代码一行不改。**

## 真实联调（等 115 测试号 Cookie）

```bash
cd ~/workspace/nextemby-replay/integration
export NB_API_KEY='上游 Emby 服务 Key'      # 必填，只进内存
export NB_115_COOKIE='115 测试号 Cookie'     # 必填，只进内存
export NB_PROBE_PATH='/CloudNAS/CloudDrive/115open/剧集/xxx/xxx.mp4'  # 可选：启动前指纹自检
go run ./cmd/livetest
```

启动后按终端打印的 4 步验证。密钥绝不打日志、不落盘。

## 待真实账号回答的三个问题（adapter115 README 已标注）

1. 秒传 `preID` 传空服务端是否接受 → 联调时看 `RapidTransfer` 日志
2. `status=7` 签名挑战触发频率 → 多次播放后统计
3. `FileSHA1ByPath`（接收目录+根目录近似全域扫描）命中率 → `NB_PROBE_PATH` 自检直接给出
