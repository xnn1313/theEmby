# nextemby-replay

NextEmby 功能复刻（clean-room 实现）：Emby 反代网关 + 网盘秒传播放决策引擎。

> 说明：本项目为功能级复刻，不包含官方 NextEmby / NextFind 的任何源码与产物。
> 115 对接使用非官方 API（基于开源 115driver），仅供学习研究。

## 架构

```
Emby 客户端 → nb-gateway（反代，:8091）→ 上游 Emby Server（元数据/媒体库）
                        ↓ 只劫持 PlaybackInfo
                   engine（播放决策引擎）
                        ↓ 秒传/直链
                   adapter115 → 115 网盘（池账号 / 种子号）
```

- **engine/**：播放决策引擎（纯逻辑，零第三方依赖）。并发策略、10min 直链缓存、
  115/池模式分流、24h 锁定路由、池账号分配（3～4 用户上限）、统计。
  外部依赖全部抽象为 interface，`engine/memory` 提供内存实现。
- **gateway/**：反代网关。透传上游 Emby + 劫持 `PlaybackInfo` 改写播放地址为
  `/nb/stream` 短链（HMAC 签发）→ 302 跳 115 直链。指纹解析 = 路径映射
  （默认 `/CloudNAS/CloudDrive/115open` → 115 根目录）+ 播放时实时取 SHA1，不预扫。
- **adapter115/**：115 适配器。Cookie 登录、SHA1 秒传、全域 SHA1 探测、取直链、
  目录管理、按路径取 SHA1。Cookie 只进构造器，永不落日志。

详细功能规格见 `../nextemby-research/SPEC.md`（研究笔记，不在 git 内）。

## 本地部署（Docker）

```bash
cp .env.example .env
# 编辑 .env：至少填写 NB_UPSTREAM 和 NB_API_KEY
docker compose up -d --build
# 首次访问：http://你的服务器IP:8091
docker compose logs -f nb-gateway
```

把 Emby 客户端的服务器地址指向 `http://你的服务器IP:8091` 即可当普通 Emby 用；
播放时会走秒传决策链（需配 115 Cookie，否则优雅降级为上游直连）。

## Web 页面

- 管理后台：`http://你的服务器IP:8091/nb/admin/`（token 见 `NB_ADMIN_TOKEN` 或容器日志）
- 个人中心：`http://你的服务器IP:8091/nb/me/?nb_user=lzy`

![管理后台](docs/admin.png)
![个人中心](docs/me.png)
（截图待补充）

## 本地开发

需要 Go 1.24+。

```bash
# 全模块构建/测试
for m in engine gateway adapter115; do
  ( cd $m && go build ./... && go test -race -count=1 ./... )
done

# 直接跑网关（不经过 Docker）
cd gateway
NB_UPSTREAM=https://embyf.bbstzb.org NB_API_KEY=你的Key go run ./cmd/nb-gateway
```

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `NB_UPSTREAM` | 是 | 上游 Emby 地址，如 `https://embyf.bbstzb.org` |
| `NB_API_KEY` | 是 | 上游服务 API Key（后台共用账号） |
| `NB_LISTEN` | 否 | 监听地址，默认 `:8091` |
| `NB_PATH_MAP` | 否 | 路径映射，默认 `/CloudNAS/CloudDrive/115open=/` |
| `NB_HMAC_KEY` | 否 | `/nb/stream` 签发密钥，不设则启动时随机 |
| `NB_115_COOKIE` | 否 | 115 池账号 Cookie（`UID=…;CID=…;SEID=…;`） |
| `NB_115_ACCOUNT` | 否 | 池账号标识，默认 `115小1` |
| `NB_115_SEED_COOKIE` | 否 | 115 种子账号 Cookie |
| `NB_115_SEED_ACCOUNT` | 否 | 种子账号标识，默认 `115大` |
| `SSL_CERT_FILE` | 否 | 自定义 CA bundle 路径 |

## 备注

- 115 Cookie 请只用自己的小号测试，不要外传；Cookie 失效时容器启动即报错。
- 持久化：用户 / Cookie（AES-GCM）/ 播放记录 / 决策日志 / 池账号存 SQLite
  （`NB_DB_PATH`，docker 里为 `/data/nextemby.db` + volume）；直链缓存、24h 路由锁、
  并发计数为 transient 内存状态。
- 当前为 MVP：神盾/NextFind 对接、订阅追更、洗版等模块尚未实现。
