# 反代网关（模块2 · B 模式）

NextEmby 复刻项目的反代网关：对外暴露 Emby 兼容接口，背后反代一台真正的
上游 Emby Server，大部分请求原样透传，只把播放链路（PlaybackInfo）劫持掉，
换成秒传 + 直链（SPEC §2 总体架构）。

```
Emby 客户端 ──► 网关 :8091 ──► 上游 Emby（透传：登录/浏览/海报/搜索/进度上报）
                    │
                    │ 劫持 POST /emby/Items|Videos/{id}/PlaybackInfo
                    ▼
              FingerprintResolver（路径映射 + 播放时实时取 SHA1）
                    │ 查到
                    ▼
              engine.Handle（播放决策引擎：并发/缓存/115三步/池模式）
                    │ 成功
                    ▼
        MediaSource.DirectStreamUrl := /nb/stream?t=<HMAC token>
                    │
                    ▼
        客户端 GET /nb/stream?t=… → 验签 → 302 → 115 直链（10min 缓存）
```

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `NB_UPSTREAM` | 是 | 上游 Emby 地址，如 `https://emby.example.com` |
| `NB_API_KEY` | 是 | 上游服务 API Key（后台共用账号拉元数据；**只存内存，绝不打日志**） |
| `NB_LISTEN` | 否 | 监听地址，默认 `:8091`（对标官方端口） |
| `NB_HMAC_KEY` | 否 | `/nb/stream` 签发密钥；未设置则生成一次性密钥并告警（重启后旧 token 失效） |
| `NB_PATH_MAP` | 否 | Emby库路径 → 115网盘路径映射，见下 |
| `NB_ADMIN_TOKEN` | 否 | 管理后台 token；未设置则启动时随机生成并**打印到日志**（docker logs 可见） |
| `NB_COOKIE_KEY` | 否 | 用户 Cookie 加密密钥；未设置则启动时随机生成（只告警，**不打印密钥本身**） |
| `NB_DB_PATH` | 否 | SQLite 路径，默认 `./nextemby.db`；docker 里用 `/data/nextemby.db`（+ volume） |
| `SSL_CERT_FILE` | 否 | egress CA bundle 路径；不设则尝试 `/run/hatch/egress-tls/ca-bundle.pem`，再回退系统证书池 |

## 持久化（SQLite）

`store` 模块（pure Go，`modernc.org/sqlite`，distroless 静态构建可用）。
表：`users` / `cookies`（AES-GCM，nonce 12B‖密文，密钥规整与内存版一致）/
`playback_records` / `decisions`（每次 `Handle` 经 `Engine.OnDecision` 同步写入）/
`pool_accounts`。启动建表（`CREATE TABLE IF NOT EXISTS`），`EnsureDefaults`
种子写入演示用户与池账号。

管理后台的统计/用户/播放日志与个人中心的统计改读 DB；`PoolLocks`（24h 路由锁）、
并发会话数、10min 直链缓存仍在 engine 内存——transient 状态，重启丢失只触发
一次重新探测，不影响正确性。未接 DB 时各 handler 回退到内存视图（既有测试零改动）。

### NB_PATH_MAP 示例

```bash
export NB_PATH_MAP="/CloudNAS/CloudDrive/115open=/;/mnt/tv=/115/剧集"  # 默认第一条，可不配
```

多条规则用 `;` 分隔，每条 `emby前缀=115前缀`，最长前缀优先。
默认 `NB_PATH_MAP="/CloudNAS/CloudDrive/115open=/"`（nextemby 形态）：
Emby 的 `MediaSource.Path` 为 `/CloudNAS/CloudDrive/115open/电影/去有风的地方.S01E04.mp4` 时，
映射为 115 根目录下的 `/电影/去有风的地方.S01E04.mp4`，播放时实时取其 SHA1。

## 指纹解析设计（FingerprintResolver）

SHA1 无法从 Emby 播放地址推导（URL 不带内容哈希，现算需下载全文件，
违背秒传）。实际做法（2026-10-09 用户确认）：

1. **不预扫建快照**：播放时把 Emby 路径经映射规则换算成 115 路径，
   实时调 115 文件列表 API 取元数据的 sha1 字段
  （`adapter115.Client.FileSHA1ByPath`）；
2. 网关拿到 `MediaSource.Path` → `PathMapper` 按 NB_PATH_MAP 映射 →
   `LiveFingerprintResolver` 实时取 SHA1。

查不到指纹时**优雅降级**：原样返回上游 PlaybackInfo，客户端走上游直连播放。

### 与 adapter115 的对接点

`SHA1Fetcher` 接口（见 fingerprint.go）已由
`adapter115.Client.FileSHA1ByPath(ctx, path115) (string, error)` 实现：
用 `DirCID` + `ListDir` 按路径查元数据 sha1 字段。把该 Client 作为 fetcher
注入 `NewLiveFingerprintResolver` 即可，网关侧无需改动。

## 行为细节

- **透传**：除劫持点外所有请求经 `httputil.ReverseProxy` 原样转发（保留
  path/query/headers）；缺 `api_key` 时补服务 Key，客户端自带的不覆盖；
  客户端 Emby 认证头原样转发；剥离网关内部头 `X-NB-User`。
- **用户身份**：`X-NB-User` header 优先，其次 `?nb_user=` 参数，缺省 `guest`。
  演示用户：`lzy`（池模式/vip）、`guest`（池模式），见 `cmd/nb-gateway/main.go`
  的 `buildDemoEngine`；将来接真实用户库时替换 `UserStore` 实现即可。
- **劫持**：`POST /emby/Items/{id}/PlaybackInfo`、`/emby/Videos/{id}/PlaybackInfo`
 （/emby 前缀与根路径都支持）。一次请求只对首个可解析的 MediaSource 跑决策
  （= 占用一个并发会话）；并发拒绝返回 429 `{"error":"concurrency_limit"}`；
  引擎异常则降级透传。
- **会话释放（MVP）**：决策成功后会话保持到 token 过期（2h）后释放
  （`time.AfterFunc`）；后续可接 `Sessions/Playing/Stopped` 做精确释放。
- **/nb/stream**：token = `base64url(json).hex(hmac-sha256)`，载荷仅含
  exp/acct/sha1，有效期 2h。验签失败/过期 → 403。网关侧 10min 直链缓存；
  未命中则经 `DriveClient.DirectURL` 现取；**302 重定向**到 115 直链
  （省网关带宽，Emby 客户端跟随跳转）。
- **日志纪律**：所有日志经 `sanitizeURL` 脱敏，`api_key` 打码为 `***`；
  有回归测试保证 Key 不出现在任何日志输出。

## 联调步骤（真实上游）

```bash
export NB_UPSTREAM="https://emby.example.com"
export NB_API_KEY="你的服务Key"          # 只放环境变量，不进代码不进日志
export NB_LISTEN=":8091"
export NB_HMAC_KEY="$(head -c32 /dev/urandom | base64)"  # 生产固定
export NB_PATH_MAP="/emby/media=/115/媒体库"             # 按实际库路径改
# 本 VM 出站 TLS 被 egress 代理 MITM：SSL_CERT_FILE 缺省会自动信任
# /run/hatch/egress-tls/ca-bundle.pem；换环境时用 SSL_CERT_FILE 指定

go build -o nb-gateway ./cmd/nb-gateway
./nb-gateway
```

1. 先验证透传：`curl http://127.0.0.1:8091/emby/System/Info/Public` 应回上游 JSON。
2. 配指纹：把 `adapter115.Client`（需 115 Cookie）作为 fetcher 注入
   `NewLiveFingerprintResolver`，或先用假 fetcher 做冒烟。
3. 播放劫持验证：Emby 客户端把服务器地址指向网关，点播 → 网关日志应出现
   `playbackinfo user=… branch=… account=…`；客户端实际拉的是 `/nb/stream`
   302 后的 115 直链。
4. 切真实 115：设置 `NB_115_COOKIE`（池账号）/ `NB_115_SEED_COOKIE`（种子号），
   重启即生效（启动时 LoginCheck，Cookie 失效直接报错退出）。

## 未定点

- 多 MediaSource 只决策首个可解析源，其余保持上游原样作 fallback。
- 神盾"命中 → 转存"分支按 API 文档推断实现（真实日志只观测到未命中分支）。
- 会话释放目前是 2h 定时，精确释放待接 `Sessions/Stopped`。
- WebSocket 未特殊处理（Emby 部分功能用，如通知）；MVP 范围外。
- 用户体系/Cookie/播放记录/决策日志/池账号已接入 SQLite（`store` 模块，pure Go），
  重启不丢失。transient 状态（10min 直链缓存、24h 路由锁、并发会话计数）仍在内存，
  重启丢失只触发一次重新探测，不影响正确性。

## Web 页面（管理后台 + 个人中心）

零第三方依赖：Go `embed` 内嵌两个单文件页面（vanilla JS + fetch）。

- **管理后台** `GET /nb/admin/`：鉴权 = 密码登录（session cookie，
  `POST /nb/admin/api/login`，首次访问设置密码）或 `NB_ADMIN_TOKEN`
 （`Authorization: Bearer` 或 `?token=`；未设置时启动日志打印随机 token）。
  - 总览：今日各分支决策计数、直链缓存命中率、池账号锁定占用
  - 用户：列表（模式/套餐/并发/成功失败/Cookie 状态），可改模式与模板
  - 池账号：健康/锁定用户数/上限，可手动摘除/恢复（调 `SetHealthy`）
  - 播放日志：最近 N 条决策（用户/分支/账号/耗时）
  - 配置查看：上游地址、路径映射、监听地址（API Key 永不输出）
- **个人中心** `GET /nb/me/`：身份经 `?nb_user=`（MVP，后续接登录）。
  - 我的 115 Cookie：填写/更新，保存前做 115 LoginCheck 校验，
    AES-GCM 加密存储（`gateway/cookies.go`，密钥 `NB_COOKIE_KEY`），Cookie 永不打日志
  - 模式切换：用自己的 115 / 走分布式池（切 115 且无 Cookie 时提示）
  - 我的统计：直连成功/失败计数、近期播放记录

JSON API（管理后台统一 `/nb/admin/api/` 前缀 + 鉴权；个人中心 `/nb/me/api/`）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/nb/admin/api/stats` | 今日分支统计、缓存命中率、池占用 |
| GET | `/nb/admin/api/users` | 用户列表 |
| PUT | `/nb/admin/api/users/{id}` | 改模式/模板 `{mode, template}` |
| GET | `/nb/admin/api/pool` | 池账号列表 |
| POST | `/nb/admin/api/pool/{id}/health` | `{healthy}` 摘除/恢复 |
| GET | `/nb/admin/api/decisions?limit=50` | 播放日志 |
| GET | `/nb/admin/api/config` | 配置查看（脱敏） |
| GET | `/nb/me/api/profile?nb_user=` | 个人档案 + 统计 + 近期播放 |
| POST | `/nb/me/api/cookie?nb_user=` | `{cookie}` 保存（先 LoginCheck） |
| POST | `/nb/me/api/mode?nb_user=` | `{mode}` 切换模式 |

## 管理后台 API（可配置项）

所有后台页面（控制台/用户管理/服务器配置/系统日志/缓存列表/系统设置）
都经本节 API 读写配置，配置持久化在 SQLite，重启不丢失。

### 鉴权

`POST /nb/admin/api/login`（不鉴权）、`GET /nb/admin/api/login/status`
（不鉴权）、`POST /nb/admin/api/logout`（不鉴权）之外的全部
`/nb/admin/api/*` 需鉴权，三选一：

1. 登录 session：登录成功后 `Set-Cookie: nb_session=...`（HttpOnly，
   `Path=/nb/admin/`，`SameSite=Lax`，24h 有效）；
2. `Authorization: Bearer <NB_ADMIN_TOKEN>`；
3. `?token=<NB_ADMIN_TOKEN>`。

`webUI` 未启用时全部返回 404。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/nb/admin/api/login` | `{password}` 登录；首次 `{new_password}` 设置密码；`{password,new_password,change:true}`（已登录）改密码。成功 `{ok:true}` + session cookie；失败 401 `{error}` |
| GET | `/nb/admin/api/login/status` | `{passwordSet}` 是否已设置管理密码 |
| POST | `/nb/admin/api/logout` | 删 session、清 cookie，`{ok:true}` |

密码 hash 格式 `v1$<saltHex>$<hex(sha256(salt+password))>`（salt 16 字节随机），
存在 `settings.admin.password_hash`，**永不返回、永不打日志**。

### 完整 API 表

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/nb/admin/api/stats` | 今日分支统计、缓存命中率、池占用 |
| GET | `/nb/admin/api/decisions?limit=50` | 播放决策日志（DB 全量） |
| GET | `/nb/admin/api/config` | 配置查看：upstream/listen/pathMap（脱敏） |
| GET | `/nb/admin/api/settings?prefix=` | 配置键值（`admin.password_hash` 永不返回；`nextfind.agent_key` 掩码；附带 `admin.cookie_bound`: "1"/"0"） |
| PUT | `/nb/admin/api/settings` | `{k:v}` 批量写；拒绝写 `admin.password_hash`（400） |
| GET | `/nb/admin/api/accounts` | 115 账号列表：`{id,name,kind,hasCookie,cookieMask,uid,maxUsers,lockedUsers,healthy,enabled,rapidDir,quota}`（cookie 明文永不返回） |
| POST | `/nb/admin/api/accounts` | `{id*,name,kind*,cookie,uid,maxUsers,enabled,rapidDir}`；cookie 非空先 LoginCheck，失效 400；kind 非法 400 |
| PUT | `/nb/admin/api/accounts/{id}` | patch（只更新提供的字段；cookie 非空先校验再存，不提供则保留旧值） |
| DELETE | `/nb/admin/api/accounts/{id}` | 删除账号（含 cookie 密文） |
| GET | `/nb/admin/api/pathmaps` | 路径映射 `[{embyPath,accountId,subPath}]` |
| POST | `/nb/admin/api/pathmaps` | `{embyPath*,accountId*,subPath}`；accountId 必须在 accounts115 存在；改动后自动 ReloadPathMaps（热更新） |
| DELETE | `/nb/admin/api/pathmaps/{embyPath}` | embyPath 需 URL 编码；改动后自动 ReloadPathMaps |
| GET | `/nb/admin/api/templates` | 并发/额度策略模板列表 |
| POST | `/nb/admin/api/templates` | body 即模板对象，`{name*,maxConcurrent,maxDevices,defaultLine,dailyPlays,uidTaskLimit,lockHours,dailyRapid,giftDays,expireDeleteDays}` |
| PUT | `/nb/admin/api/templates/{name}` | body 即模板对象（以路径 name 为准） |
| DELETE | `/nb/admin/api/templates/{name}` | 不允许删最后一个模板（400） |
| GET | `/nb/admin/api/users` | 用户列表：`{id,mode,template,sessions,hasCookie,ok,fail,expiresAt,banned,remark,plays30d,lastUA,drive,driveOk}`（drive: 115 模式"自备网盘"/池模式"负载均衡"） |
| POST | `/nb/admin/api/users` | `{id*,mode,template,expiresAt,remark}`；重复 id → 409 |
| PUT | `/nb/admin/api/users/{id}` | patch：`{mode,template,expiresAt,banned,remark}`（只更新提供的字段） |
| DELETE | `/nb/admin/api/users/{id}` | 删除用户 |
| GET | `/nb/admin/api/logs?category=&limit=200` | 系统日志 `[{id,ts,category,message}]`（limit 上限 1000） |
| DELETE | `/nb/admin/api/logs` | 清空系统日志 |
| GET | `/nb/admin/api/cache` | 直链缓存 `[{user,filename,accountUID,ua,expiresAt,sha1}]` |
| DELETE | `/nb/admin/api/cache` | 清空直链缓存 |
| DELETE | `/nb/admin/api/cache/{user}/{sha1}` | 删除单条缓存，`{ok}` |
| GET | `/nb/admin/api/console?range=24h\|7d\|30d` | 控制台：播放聚合 + 系统状态（见下） |
| POST | `/nb/admin/api/restart` | `{ok:true}` 后 500ms 进程退出（由 supervisor/docker 拉起） |

### console 返回

```json
{
  "range": "24h", "total": 120,
  "transfer": 40, "direct": 75, "cookie": 12,
  "sourceRate": 0.97,
  "cpu": 12.5, "memMB": 38.4, "diskUsed": 123456, "diskTotal": 987654,
  "timeline": [{"t": 1728384000, "plays": 5}],
  "uaStats": [{"name": "Filmly", "plays": 60, "directs": 40}]
}
```

- `transfer`：秒传类分支（p2p_rapid/seed_fallback/pool_shield_transfer/pool_seed_fallback）
- `direct`：直连类分支（cache_hit/own_drive_hit/pool_probe_hit）
- `cookie`：115 模式相关分支（own_drive_hit/p2p_rapid/seed_fallback）
- `sourceRate`：成功率 OK/Total
- `uaStats`：UA 归一到 Filmly / VidHub / 网易爆米花 / Emby / 其他，按 plays 倒序
- `cpu`：/proc/stat 两次采样（50ms 间隔）使用率，非 Linux 为 0

### settings 键表

| key | 说明 | 生效 |
|---|---|---|
| `emby.addr` / `emby.port` | 上游 Emby 地址/端口（覆盖 `NB_UPSTREAM`，scheme 沿用） | 重启 |
| `emby.proxy_port` | 网关监听端口（覆盖 `NB_LISTEN`，如 `8091`） | 重启 |
| `nextfind.agent_key` | 神盾/NextFind OpenAPI 密钥（读取时掩码展示） | 实时读 |
| `admin.password_hash` | 管理密码 hash（只经 login API 写，永不返回） | 实时 |
| `admin.cookie_bound` | 只读加算：是否有 kind=seed 且已绑 cookie 的账号 | — |

### 路径映射热更新

`ReloadPathMaps()`：DB `path_maps` 非空则用 DB（`sub_path` 空→`"/"`，
按 Emby 前缀长短排序沿用 `ParsePathMap` 逻辑），否则回退 `NB_PATH_MAP`
环境变量解析。pathmaps 的 POST/DELETE 后自动调用，指纹解析器内部换成
`DynamicMapper`（每次解析取最新规则），无需重启。

### 重启

`POST /nb/admin/api/restart` 先回 `{ok:true}`，500ms 后 `os.Exit(0)`。
docker-compose 下容器自动拉起；systemd 部署需配 `Restart=always`。
