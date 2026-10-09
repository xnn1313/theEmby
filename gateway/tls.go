package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// hatchCABundle 是本 VM egress 代理 MITM 证书的默认位置。
// 出站 TLS 会被 egress forward proxy 拦截，直接握手会失败，
// 因此 Go 必须信任该 CA。不要写死：SSL_CERT_FILE 环境变量优先，
// 其次尝试本默认路径，最后回退到系统证书池（见 newTransport）。
const hatchCABundle = "/run/hatch/egress-tls/ca-bundle.pem"

// newTransport 构造外发 HTTP Transport。
//
// 证书优先级：SSL_CERT_FILE > hatch 默认 bundle（存在时）> 系统证书池。
// 代理走 http.ProxyFromEnvironment，即复用 https_proxy 环境变量，
// 以穿过 egress forward proxy。
func newTransport(certFile string) (*http.Transport, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	bundle := certFile
	if bundle == "" {
		if _, err := os.Stat(hatchCABundle); err == nil {
			bundle = hatchCABundle
		}
	}
	if bundle != "" {
		pem, err := os.ReadFile(bundle)
		if err != nil {
			return nil, fmt.Errorf("gateway: 读取 CA bundle %q 失败: %w", bundle, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("gateway: CA bundle %q 无有效证书", bundle)
		}
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	return t, nil
}
