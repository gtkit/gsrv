package configloader

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// LoopbackHost 只接受loopback字面量或localhost，不通过DNS猜测内网信任。
func LoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// ClientTLS 保持服务器证书校验；空CA使用系统信任，额外CA仅添加到本客户端池。
func ClientTLS(caFile, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile == "" {
		return cfg, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("client TLS system roots: %w", err)
	}
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("client TLS CA file: %w", err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("client TLS: CA file contains no certificates")
	}
	cfg.RootCAs = pool
	return cfg, nil
}
