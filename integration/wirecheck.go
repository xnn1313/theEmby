package integration

// wirecheck.go — 三个模块的"插头对得上"证明。
//
// 模块 1（engine）把外部依赖抽象成接口，模块 3（adapter115）的真实实现
// 必须能直接插进模块 1 的 Deps、模块 2（gateway）的指纹解析器。
// 下面两行是编译期断言：接口一旦漂移，这里先编译失败，而不是联调时才炸。
// 真实 115 联调 = 把测试里的 memory 假实现换成 adapter115.New 出来的 Client，
// 其余代码一行不改。

import (
	"nextemby-replay/adapter115"
	"nextemby-replay/engine"
	"nextemby-replay/gateway"
)

var (
	// adapter115.Client 可直接作为引擎的池账号/种子账号 DriveClient。
	_ engine.DriveClient = (*adapter115.Client)(nil)
	// adapter115.Client.FileSHA1ByPath 可直接作为网关的实时指纹抓取器。
	_ gateway.SHA1Fetcher = (*adapter115.Client)(nil)
)
