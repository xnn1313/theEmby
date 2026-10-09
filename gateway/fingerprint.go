package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// FingerprintResolver 按上游 Emby 的 MediaSource.Path 解析文件 SHA1 指纹。
//
// 设计依据（2026-10-09 用户确认）：
//   - SHA1 无法从 Emby 播放地址推导（URL 不带内容哈希，现算需下载全文件，违背秒传）；
//   - 不预扫建快照：播放时把 Emby 路径经映射规则换算成 115 路径，实时调 115
//     文件列表 API 取元数据的 sha1 字段（adapter115.Client.FileSHA1ByPath）。
//   - nextemby 的真实映射：Emby 路径中的 /CloudNAS/CloudDrive/115open 对应 115 根目录。
//
// 查不到时返回 ok=false，网关优雅降级：原样返回上游 PlaybackInfo 响应，
// 客户端走上游直连播放，不中断。
type FingerprintResolver interface {
	ResolveSHA1(ctx context.Context, embyPath string) (sha1 string, ok bool)
}

// ---------------------------------------------------------------- 路径映射

// PathRule 是一条 "Emby库路径前缀 → 115网盘路径前缀" 映射规则。
type PathRule struct {
	EmbyPrefix string // 如 "/CloudNAS/CloudDrive/115open"
	Path115    string // 如 "/"（115 根目录）
}

// ParsePathMap 解析 NB_PATH_MAP 环境变量，格式：
//
// "/CloudNAS/CloudDrive/115open=/;/mnt/tv=/115/剧集"
//
// 多条规则用 ";" 分隔，每条为 "emby前缀=115前缀"。空字符串返回空规则集。
func ParsePathMap(s string) ([]PathRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var rules []PathRule
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" || strings.TrimSpace(kv[1]) == "" {
			return nil, fmt.Errorf("gateway: 路径映射规则格式错误 %q，应为 \"emby前缀=115前缀\"", part)
		}
		rules = append(rules, PathRule{
			EmbyPrefix: strings.TrimSuffix(strings.TrimSpace(kv[0]), "/"),
			Path115:    strings.TrimSuffix(strings.TrimSpace(kv[1]), "/"),
		})
	}
	// 最长前缀优先：EmbyPrefix 长的排前面。
	sort.Slice(rules, func(i, j int) bool {
		return len(rules[i].EmbyPrefix) > len(rules[j].EmbyPrefix)
	})
	return rules, nil
}

// PathMapper 按规则把 Emby 库路径映射为 115 网盘路径。
type PathMapper struct {
	rules []PathRule
}

// NewPathMapper 创建映射器（rules 为 nil 表示无映射，全部 miss）。
func NewPathMapper(rules []PathRule) *PathMapper {
	return &PathMapper{rules: rules}
}

// Map115 返回 embyPath 对应的 115 路径；无规则命中返回 ok=false。
func (m *PathMapper) Map115(embyPath string) (string, bool) {
	for _, r := range m.rules {
		p := r.EmbyPrefix
		if p == "" {
			continue
		}
		if embyPath == p || strings.HasPrefix(embyPath, p+"/") {
			rest := embyPath[len(p):]
			if rest == "" {
				rest = "/"
			}
			return r.Path115 + rest, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- 实时解析

// SHA1Fetcher 按 115 网盘路径实时取文件 sha1。
//
// 生产实现：adapter115.Client.FileSHA1ByPath（调 115 文件列表 API，
// 用元数据的 sha1 字段）。返回 error 视为未命中。
type SHA1Fetcher interface {
	FileSHA1ByPath(ctx context.Context, path115 string) (sha1 string, err error)
}

// LiveFingerprintResolver 是生产形态的解析器：
// embyPath → 路径映射 → 实时调 115 取 sha1。不预扫，无快照。
type LiveFingerprintResolver struct {
	mapper  *PathMapper
	fetcher SHA1Fetcher // nil 时恒 miss（优雅降级）
}

// NewLiveFingerprintResolver 创建解析器。fetcher 为 nil 时恒 miss。
func NewLiveFingerprintResolver(mapper *PathMapper, fetcher SHA1Fetcher) *LiveFingerprintResolver {
	if mapper == nil {
		mapper = NewPathMapper(nil)
	}
	return &LiveFingerprintResolver{mapper: mapper, fetcher: fetcher}
}

// ResolveSHA1 实现 FingerprintResolver。
func (r *LiveFingerprintResolver) ResolveSHA1(ctx context.Context, embyPath string) (string, bool) {
	path115, ok := r.mapper.Map115(embyPath)
	if !ok || r.fetcher == nil {
		return "", false
	}
	sha1, err := r.fetcher.FileSHA1ByPath(ctx, path115)
	if err != nil || sha1 == "" {
		return "", false
	}
	return sha1, true
}
