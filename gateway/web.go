package gateway

import "embed"

// webFS 内嵌管理后台与个人中心页面（零第三方依赖，单二进制）。
//
//go:embed web/admin.html web/me.html
var webFS embed.FS
