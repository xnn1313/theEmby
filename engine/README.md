# 播放决策引擎（模块 1）

NextEmby 复刻项目的纯逻辑层：给定一次播放请求，决策出走哪条链路、
用哪个 115 账号、返回哪条直链。零第三方依赖（标准库 only），所有外部
I/O 全部抽象成 Go interface，`memory` 子包提供 in-memory 实现，
不依赖任何外部服务即可运行与测试。

行为依据：`~/workspace/nextemby-research/SPEC.md`（§9 分布式池调度、
§10 神盾、§12 完整播放链路图）。clean-room 实现：仅参考
PivKeyU/Emotion 的设计思想，未复制其代码。

## 目录结构

```
engine/
├── go.mod            # module nextemby-replay/engine, go 1.24, 零第三方依赖
├── types.go          # PlaybackRequest / Decision / StepTrace / User / PoolAccount
├── interfaces.go     # DriveClient / ShieldClient / PlaybackRecordStore /
│                     #   UserStore / PoolStore
├── engine.go         # Engine：并发准入、缓存、24h锁、池分配、两条模式链路、统计
├── memory/
│   └── memory.go     # 所有接口的 in-memory 实现（测试/演示/本地联调）
├── engine_test.go    # 13 个单测（mock 全部外部接口）
└── README.md
```

## 核心入口

```go
eng := engine.New(cfg, engine.Deps{
    Users:   userStore,              // NextEmby 侧用户
    Pool:    poolStore,              // 池账号列表与健康状态
    Seed:    seedDrive,              // 115大（种子账号）
    Own:     map[string]engine.DriveClient{...}, // userID -> 自有115（115模式）
    PoolDrv: map[string]engine.DriveClient{...}, // accountID -> 池账号（池模式）
    Shield:  shieldClient,           // NextFind 神盾
    Records: recordStore,            // 播放记录库
})

d, err := eng.Handle(ctx, engine.PlaybackRequest{
    UserID: "lzy", FileSHA1: "A2375D8E...", FileName: "...mp4",
    Client: "Filmly", TMDBID: "935597", MediaType: "movie",
})
// d.Allowed / d.Branch / d.DirectURL / d.AccountID / d.Steps
// d.Release() 必须在播放结束（或放弃）时调用，否则会话计数泄漏。
```

`Decision.Steps` 记录每一步的分支与耗时（`Decision.Summary()` 输出单行摘要），
对标真实播放日志格式，网关层可直接打日志。

## 决策链（与 SPEC §12 对应）

```
Handle
 ① 并发策略：按用户模板（如 vip→上限5）做准入，超限返回
    Allowed=false（正常决策，不是 error）；通过则会话数+1
 ② 定服务账号（本地路由）：115模式→用户自有盘；池模式→24h锁命中则沿用，
    否则分配到锁定用户数最少的健康账号（上限默认4，可配）
 ③ 直链缓存：key=(账号,SHA1)，TTL 10min，命中直接返回
 ④ 模式链路：
    115模式：STEP1 自有盘SHA1探测 → STEP2 播放记录库用户间秒传
             → STEP3 源盘兜底（115大）
    池模式：全域SHA1探测 → 命中取直链
            → 未命中→神盾查库(sha1)→命中则转存进池(/最近接收)
            → 未命中→后台神盾主动搜索(TMDB)+源网盘秒传兜底
 ⑤ 收尾：写10min缓存、记播放记录（供STEP2）、统计成功/失败计数
```

## 接口与未来真实实现的对应位置

| Interface | 将来真实实现 | 位置 |
|---|---|---|
| `DriveClient` | 115 适配器：Cookie 登录、SHA1 秒传、取直链、目录管理（115 非官方 API） | 模块 3 |
| `ShieldClient` | NextFind OpenAPI：`GET /shield/search`、`POST /transfer`、后台搜索（`X-API-Key` 鉴权） | 后续模块 |
| `PlaybackRecordStore` | 播放记录库持久化（file_hash → 最近播放用户） | 后续模块（DB） |
| `UserStore` | NextEmby 用户体系（注册/模板/模式开关） | 后续模块（DB） |
| `PoolStore` | 池账号表（115小N 列表、健康状态由 API 监控写入） | 后续模块（DB） |

`Engine.SetPoolAccountHealthy` 已预留给将来的 API 监控调用。

## 设计决策

1. **缓存 key 需要账号，所以"定账号"先于"查缓存"**：SPEC §12 写的是
   ②缓存→③分流，但缓存 key=(账号,SHA1)，账号只能来自本地路由。
   定账号是纯本地操作（锁表/分配），无 I/O，因此把它折进"模式分流"的
   本地路由步骤，不违背 SPEC 语义。
2. **秒传是哈希操作**：`RapidTransfer(sha1, fileName)` 只需要哈希，
   "115大 → 115小2"只是决策追踪里的来源标注（`src=seed:115大`），
   不是跨账号 API 调用——这与 115 服务端全局去重的真实机制一致。
3. **115 模式 STEP2 与 STEP3 调的是同一个秒传**：区别只在来源标注
  （`src=p2p:<用户>` vs `src=seed:115大`），符合 README"零 API 消耗"
   的描述（STEP2 的查询是本地播放记录库）。
4. **24h 锁只在秒传/转存成功后建立或续期**：探测命中不续期，
   让冷文件自然过期进入 GC（SPEC §9）。
5. **神盾后台搜索是尽力而为**：失败只记入 trace，不导致播放失败；
   播放记录写入失败同理（它只服务未来的 STEP2）。
6. **拒绝是正常决策**：并发超限、池耗尽、未知模式都返回
   `Allowed=false` 的 Decision，只有 I/O 失败才返回 error。
7. **时钟注入**：`Config.Now` 可替换，缓存/锁的 TTL 测试是确定性的。

## 未定点（复刻时需确认）

- 神盾"命中→转存"分支是按 API 文档推断的，真实日志只观测到未命中分支。
- Pro 模式在池模式下是否走三级加速，SPEC 待澄清（本引擎实现的是日志实测的普通模式链路）。
- 池账号"3~4 用户"上限在 `PoolAccount.MaxUsers` 可配，默认 4。
- 24h 锁过期后的 GC 策略（删文件/保留）不在本模块范围，由池运维模块决定。

## 测试

```sh
export PATH=$HOME/workspace/toolchain/go/bin:$PATH
go vet ./... && go build ./... && go test ./...
```

13 个单测覆盖：并发超限拒绝与释放、缓存命中/过期、24h 锁定路由与过期重分配、
池分配上限与不健康账号跳过、115 模式三步链路、池模式全链路
（探测命中 / 神盾命中转存 / 神盾未命中+后台搜索+源盘兜底）、统计成功/失败计数、
未知用户、未知模式。`go test -race` 通过。
