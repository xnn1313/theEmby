package engine

import "fmt"

// logf 是中文播放日志 hook 的内部入口：LogFunc 为 nil 时静默。
// category 取值 play | user | error | system。
// 注意：日志文案里绝不输出 Cookie / API Key 等敏感信息。
func (e *Engine) logf(category, format string, args ...any) {
	if e.LogFunc == nil {
		return
	}
	e.LogFunc(category, fmt.Sprintf(format, args...))
}

// formatSize 把字节数格式化为人类可读的大小，
// 如 33.79GB / 512.00MB / 900.00KB / 12B（对标官方 NextEmby 播放日志）。
func formatSize(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	for _, u := range []string{"KB", "MB", "GB"} {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.2f%s", f, u)
		}
	}
	return fmt.Sprintf("%.2fTB", f/unit)
}

// shortSHA1 取 SHA1 前 8 位用于日志展示（不足 8 位则原样返回）。
func shortSHA1(sha1 string) string {
	if len(sha1) > 8 {
		return sha1[:8]
	}
	return sha1
}
