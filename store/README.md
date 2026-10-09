# store — SQLite 持久化层

`nextemby-replay` 的持久化模块：pure Go（`modernc.org/sqlite`，无 cgo，
distroless 静态构建可用）。

## 表

| 表 | 内容 |
|---|---|
| `users` | 用户（id/mode/template/created_at/updated_at） |
| `cookies` | 用户 115 Cookie，AES-GCM 密文（nonce 12B‖ciphertext） |
| `playback_records` | 播放记录（sha1, user_id）→ 最近播放时间；支撑 115 模式 STEP2 用户间秒传 |
| `decisions` | 每次 `Handle` 的决策快照（user/branch/account/elapsed_ms/ok/steps_json/created_at） |
| `pool_accounts` | 池账号（id/max_users/healthy） |

启动时 `CREATE TABLE IF NOT EXISTS` 建表（无迁移框架）；
`EnsureDefaults()` 幂等写入演示种子：用户 `lzy`(pool/vip)、`guest`(pool)，
池账号 `115小1`/`115小2`。

## 接口实现

- `SQLiteUserStore`：`engine.UserStore` / `UserUpdater` / `UserLister`
- `SQLiteCookieStore`：方法签名与 `gateway.CookieStore` 结构一致，可直接赋值；
  加密格式与 `gateway` 内存版完全相同（密钥规整：非 16/24/32 字节 → SHA-256）
- `SQLitePlaybackRecordStore`：`engine.PlaybackRecordStore`
  （`RecordPlayback` upsert；`FindRecentPlayer` 按 `played_at` 倒序取第一个非己用户）
- `SQLitePoolStore`：`engine.PoolStore` + `EnsureAccount`（幂等）
- 决策：`LogDecision(DecisionSummary)`（供 `Engine.OnDecision` hook）；
  查询：`BranchStats(sinceUnix)`、`UserStats()`（decisions 聚合）、
  `RecentDecisions(limit)`、`RecentUserDecisions(userID, limit)`

## Transient（不入库）

以下为带 TTL 的缓存/运行时状态，重启丢失只触发一次重新探测，不影响正确性：
10min 直链缓存、24h 路由锁、并发会话计数——仍在 `engine` 内存。

## 并发

单 `*sql.DB` 共享；`PRAGMA journal_mode=WAL` + `busy_timeout=5000`。
Cookie 加解密的 `cipher.AEAD` 调用加锁保护（与 gateway 内存版一致）。

## 依赖版本说明

`modernc.org/sqlite` 高版本要求 go ≥ 1.25/1.26，本模块 pin 在
`v1.34.5` + `modernc.org/libc v1.55.3` + `golang.org/x/sys v0.33.0`，
保持 `go 1.24` 可构建。升级前先确认新版的 go 指令要求。
