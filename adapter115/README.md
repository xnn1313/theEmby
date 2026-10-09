# adapter115 — 115 网盘适配器（模块3）

实现 `engine.DriveClient` 接口，对接单个 115 网盘账号（115大 / 115小N / 用户自有盘各一个实例）。
引擎（模块1）通过该接口完成播放决策链里的网盘操作：全域 SHA1 探测、SHA1 秒传、取直链。

## 架构

```
engine.DriveClient ──► adapter115.Client ──► wireClient ──► 115 非官方 API
                     （业务决策/日志/错误分类）  （协议窄缝，可 mock）
```

- `adapter.go`：`Client`，实现 `engine.DriveClient` + 目录扩展能力（`ListDir`/`Mkdir`/`DirCID`，
  对应 NextFind 的 `/directories` 语义）。
- `wire.go`：`wireClient` 接口与 `realWire` 真实实现；`errors.go`：结构化错误。
- `adapter_test.go`：`fakeWire` 注入全部分支的单元测试（25 个），无需真实账号。

## 115 接口来源依据

线路协议基于 **[SheltonZhu/115driver](https://github.com/SheltonZhu/115driver)**（MIT 协议，
v1.3.5），即 alist 115 驱动实际采用的同一实现。只参考其设计思想、自己实现业务层，
未复制其代码。各能力的对接结论：

| 能力 | 115 接口（经 115driver） | 状态 |
|---|---|---|
| Cookie 登录/会话 | `Credential.FromCookie` 解析 `UID/CID/SEID/KID` → `ImportCredential` → `LoginCheck` | ✅ 确定 |
| SHA1 秒传 | `POST https://uplb.115.com/4.0/initupload.php`（ECDH 加密）：`fileid`=SHA1大写、`filesize`、`filename`、`target=U_1_<cid>`；返回 `status=2` 即秒传成功 | ✅ 确定（需 filesize，见限制） |
| 全域 SHA1 探测 | 115 **没有**按 sha1 查询的公开接口；实现为"接收目录 → 根目录"分页列出并比对 `Sha1` 字段（`ListPage`） | ⚠️ 近似实现，见限制 |
| 取直链 | `POST https://proapi.115.com/app/chrome/downurl`（m115 加密）：`pickcode` → `DownloadInfo.Url.Url` | ✅ 确定（有时效） |
| 按 cid 列目录 | `GET https://webapi.115.com/files`（`ListPage`） | ✅ 确定 |
| 创建目录 | POST 表单 `pid`+`cname` → 返回 cid（`Mkdir`） | ✅ 确定 |
| 路径 → cid | `DirName2CID("/最近接收")` | ✅ 确定 |

依赖 115driver 的理由：秒传与取直链是加密协议（ECDH 握手 + 自定义 token 签名 +
请求体加密），自行实现易错且 115 经常调整协议；该库持续维护且被 alist/OpenList
生产验证。依赖库 ≠ 复制代码。

## 已知限制（联调前必读）

1. **秒传必须提供文件大小**：115 `initupload` 的 `filesize` 是必填字段。
   `engine.DriveClient.RapidTransfer(ctx, sha1, fileName)` 签名里没有大小，
   因此 `Client` 通过 `WithSizeResolver` 注入 `sha1 -> 大小` 解析器
   （`PlaybackRequest.FileSize` / NextFind 资源索引是天然来源）。
   未提供时返回 `*RapidMissError{Reason: need_file_size}`，**不碰网络**。
   → 建议：后续让引擎把 `FileSize` 透传给适配器（接口小改）。
2. **preID 为空**：`initupload` 的 token 签名混入了 preID（前 128KB 的 sha1）；
   纯哈希秒传没有文件字节，只能传空。服务端是否接受待真实联调验证。
3. **range 签名挑战（status 7）**：115 偶发要求对指定字节区间做 sha1 证明；
   无文件字节时无法完成，适配器将其判为秒传未命中（`sign_check_required`），
   引擎走兜底分支。这是**预期行为**，不是 bug。
4. **探测是近似"全域"**：按目录分页扫描（单目录最多 3 页 × 1150 条），
   只覆盖接收目录 + 根目录。我们自己的秒传永远落在 `/最近接收`，
   所以系统内产生的副本一定能命中；人工放到深层目录的文件可能漏检。
5. **直链有时效**：`DirectURL` 每次返回新鲜直链；上层按 SPEC 做 10 分钟缓存，
   不要长期缓存。
6. **认证错误启发式**：除 115driver 的 sentinel 错误外，另有关键词兜底
   （中英文"未登录/过期/失效"等），见 `wire.go:asAuthError`。

## 真实账号联调步骤

> ⚠️ 没有真实账号时**不要**尝试登录；以下步骤等用户提供账号后执行。

1. 准备一个 115 账号的登录 Cookie（浏览器登录 115 后从开发者工具复制，
   形如 `UID=…;CID=…;SEID=…;KID=…`），以及一个测试目录（建议新建 `/联调测试`，
   不要用 `/最近接收`，避免污染生产目录）。
2. 构造客户端并指定测试目录与大小解析：
   ```go
   c, err := adapter115.New(ctx, "115联调", cookie,
       adapter115.WithReceiveDir("/联调测试"),
       adapter115.WithSizeResolver(func(_ context.Context, sha1 string) (int64, bool) {
           return 5497558016, true // 联调时按真实文件大小返回
       }),
   )
   ```
   `New` 会做一次 `LoginCheck`，Cookie 失效直接返回 `*AuthError`。
3. 按顺序验证（对照 SPEC §12 的池模式链路）：
   - `ProbeSHA1`：先 `false`（空目录），秒传后 `true`。
   - `RapidTransfer(sha1, name)`：用一个**确定在 115 去重池中**的大文件
     SHA1（先用 115 官方客户端秒传一次确认），期望 `status=2` → 返回 `/联调测试`。
   - `DirectURL(sha1)`：期望返回 `https://…` 直链，浏览器可直接打开播放。
   - `ListDir` / `Mkdir`：对照 NextFind `/directories` 语义。
4. 观察日志：每步打一行（`✅秒传成功 … 耗时: Nms`），耗时对标 SPEC 实测值
   （秒传 ~1.4s、直链 ~0.3s）。重点验证**限制2**（preID 为空是否被接受）
   与**限制3**（是否触发 status 7）。
5. 联调通过后，把测试目录里的文件删掉，换上正式的 `/最近接收` 与真实
   `SizeResolver`（接资源索引）。

## 日志示例

```
✅[115小2]会话建立: 115 登录态校验通过
🔄[115小2]全域SHA1探测: 未命中 sha1=A2375D8E… | 耗时: 96ms
✅[115小2]秒传成功 | 目录: /最近接收 | 文件: 去有风的地方….mp4 | 大小: 5.12GB | 耗时: 1377ms
✅[115小2]直连成功 | 文件: 去有风的地方….mp4 | 耗时: 271ms
```

Cookie 永不出现在日志与错误中（`TestCookieNeverLogged` 回归覆盖）。
