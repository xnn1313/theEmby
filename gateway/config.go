package gateway

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
)

// Config 是网关的全部配置，来源只有环境变量。
//
//	NB_UPSTREAM  上游 Emby 地址，如 https://emby.example.com（必填）
//	NB_API_KEY   上游服务 API Key，后台共用这一个账号拉元数据（必填，绝不打日志）
//	NB_LISTEN    监听地址，默认 :8091（对标官方端口）
//	NB_HMAC_KEY  /nb/stream 签发密钥；未设置则生成一次性密钥并告警
//	NB_PATH_MAP  Emby库路径→115网盘路径映射，如 "/CloudNAS/CloudDrive/115open=/;/mnt/tv=/115/剧集"；
//	            为空时默认 "/CloudNAS/CloudDrive/115open=/"（nextemby 形态：该前缀对应 115 根目录）
//	NB_ADMIN_TOKEN 管理后台 token；未设置则启动时随机生成并打印到日志（docker logs 可见）
//	NB_COOKIE_KEY  用户 Cookie 加密密钥；未设置则启动时随机生成（只告警，不打印密钥本身）
//	SSL_CERT_FILE  egress CA bundle 路径（可选，见 tls.go）
type Config struct {
	Upstream   string
	APIKey     string
	Listen     string
	HMACKey    []byte
	PathMap    string
	CertFile   string
	AdminToken string
	CookieKey  []byte
}

// LoadConfig 从环境变量加载配置。APIKey 只存内存，不落盘、不打日志。
func LoadConfig() (Config, error) {
	cfg := Config{
		Upstream: os.Getenv("NB_UPSTREAM"),
		APIKey:   os.Getenv("NB_API_KEY"),
		Listen:   os.Getenv("NB_LISTEN"),
		PathMap:  os.Getenv("NB_PATH_MAP"),
		CertFile: os.Getenv("SSL_CERT_FILE"),
	}
	if cfg.Upstream == "" {
		return cfg, fmt.Errorf("gateway: 缺少环境变量 NB_UPSTREAM")
	}
	if cfg.APIKey == "" {
		return cfg, fmt.Errorf("gateway: 缺少环境变量 NB_API_KEY")
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8091"
	}
	if cfg.PathMap == "" {
		// 默认映射（nextemby 形态）：Emby 路径中的 /CloudNAS/CloudDrive/115open 对应 115 根目录。
		cfg.PathMap = "/CloudNAS/CloudDrive/115open=/"
	}
	if k := os.Getenv("NB_HMAC_KEY"); k != "" {
		cfg.HMACKey = []byte(k)
	} else {
		// 演示方便：未配置则生成一次性密钥并告警（重启后旧 token 失效）。
		// 注意：这里只打告警文本，绝不输出密钥本身。
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return cfg, fmt.Errorf("gateway: 生成 HMAC 密钥失败: %w", err)
		}
		cfg.HMACKey = key
		log.Println("[gateway] 警告: 未设置 NB_HMAC_KEY，已生成一次性密钥（重启后 /nb/stream token 失效）")
	}
	if tok := os.Getenv("NB_ADMIN_TOKEN"); tok != "" {
		cfg.AdminToken = tok
	} else {
		// 未设置则随机生成并打印到日志：这是管理员有意可见的唯一密钥类输出
		//（docker logs 可见），API Key / Cookie / NB_COOKIE_KEY 永不打印。
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return cfg, fmt.Errorf("gateway: 生成管理后台 token 失败: %w", err)
		}
		cfg.AdminToken = fmt.Sprintf("%x", raw)
		log.Printf("[gateway] 管理后台 token（未设置 NB_ADMIN_TOKEN，已随机生成）: %s", cfg.AdminToken)
	}
	if k := os.Getenv("NB_COOKIE_KEY"); k != "" {
		cfg.CookieKey = []byte(k)
	} else {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return cfg, fmt.Errorf("gateway: 生成 Cookie 密钥失败: %w", err)
		}
		cfg.CookieKey = key
		log.Println("[gateway] 警告: 未设置 NB_COOKIE_KEY，已生成一次性密钥（重启后已存 Cookie 无法解密）")
	}
	return cfg, nil
}
