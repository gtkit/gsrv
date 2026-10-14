package bootstrap

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/gtkit/gsrv"

	"github.com/gtkit/gsrv/internal/platform/config"
)

// servers 持有本服务装配结果；生命周期实现位于 internal/pkg/lifecycle。
type servers struct {
	app     *gsrv.App
	http    *gsrv.HTTPServer
	grpc    *gsrv.GRPCServer
	backend *gsrv.GRPCServer
	gateway *gsrv.Gateway
}

// buildServers 在原 serve 层级完成协议接线，构造阶段不监听端口。
func buildServers(cfg *config.Config) (*servers, error) {
	authCfg := gsrv.AuthConfig{Service: cfg.Name, Credentials: cfg.Credentials}
	if cfg.HTTP != nil {
		authCfg.MaxBodyBytes = cfg.HTTP.MaxBodyBytes
	}
	verifier, err := gsrv.NewVerifier(authCfg)
	if err != nil {
		return nil, err
	}
	quota, err := gsrv.NewLocalLimiter(cfg.Name+":"+rand.Text(), cfg.RateLimitQPS)
	if err != nil {
		return nil, err
	}
	state := gsrv.NewHealth()
	public, backend, err := buildGRPCServers(cfg, state, verifier, quota)
	if err != nil {
		return nil, err
	}
	front, bridge, err := buildHTTPServer(cfg, state, verifier, quota)
	if err != nil {
		return nil, err
	}
	registered := make([]gsrv.Server, 0, 3)
	if front != nil {
		registered = append(registered, front)
	}
	if public != nil {
		registered = append(registered, public)
	}
	if backend != nil {
		registered = append(registered, backend)
	}
	app, err := gsrv.New(cfg.Name, gsrv.WithServers(registered...), gsrv.WithHealth(state), gsrv.WithShutdownTimeout(cfg.ShutdownTimeout))
	if err != nil {
		if bridge != nil {
			err = errors.Join(err, bridge.Close())
		}
		return nil, err
	}
	return &servers{app: app, http: front, grpc: public, backend: backend, gateway: bridge}, nil
}

// Serve 托管 SIGINT/SIGTERM；HTTP→公开gRPC→内部gRPC 顺序排空。
func (s *servers) Serve(ctx context.Context) error { return s.app.RunSignals(ctx) }

// Close 在 App 退出后释放自持 gateway 连接；logger 最后 Sync。
func (s *servers) Close() error {
	if s.gateway != nil {
		return s.gateway.Close()
	}
	return nil
}
