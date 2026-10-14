package bootstrap

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gtkit/gsrv"

	"example.com/service/internal/platform/config"
	pb "example.com/service/pb/info/v1"
)

// buildHTTPServer 沿用网关装配位置；共享已认证 AppKey、配额及健康状态。
func buildHTTPServer(cfg *config.Config, state *gsrv.HealthState, verifier *gsrv.Verifier, quota gsrv.RateLimiter) (front *gsrv.HTTPServer, bridge *gsrv.Gateway, err error) {
	if cfg.HTTP == nil {
		return nil, nil, nil
	}
	handler := http.NotFoundHandler()
	if cfg.GatewaySocket != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.GatewaySocket), 0o700); err != nil {
			return nil, nil, err
		}
		bridge, err = gsrv.NewGateway(cfg.GatewaySocket, pb.RegisterInfoServiceHandler)
		if err != nil {
			return nil, nil, err
		}
		handler = bridge
	}
	front, err = gsrv.NewHTTPServer(cfg.Name, *cfg.HTTP, handler, verifier, quota, state)
	if err != nil {
		if bridge != nil {
			err = errors.Join(err, bridge.Close())
		}
		return nil, nil, err
	}
	return front, bridge, nil
}
