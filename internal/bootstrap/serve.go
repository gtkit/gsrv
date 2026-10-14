package bootstrap

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/gtkit/gsrv"

	"github.com/gtkit/gsrv/internal/platform/config"
	infort "github.com/gtkit/gsrv/internal/runtime/module/info"
)

// servers 持有本服务装配结果；生命周期实现位于 internal/pkg/lifecycle。
type servers struct {
	app     *gsrv.App
	http    *gsrv.HTTPServer
	grpc    *gsrv.GRPCServer
	backend *gsrv.GRPCServer
	gateway *gsrv.Gateway
	metrics *gsrv.Metrics
	module  *infort.Components
}

// buildServers 在原 serve 层级完成协议接线，构造阶段不监听端口。
func buildServers(cfg *config.Config) (result *servers, resultErr error) {
	authCfg := gsrv.AuthConfig{Service: cfg.Name, Credentials: cfg.Credentials, NonceCapacity: cfg.Auth.NonceCapacity, NonceCapacityPerAppKey: cfg.Auth.NonceCapacityPerAppKey}
	if cfg.HTTP != nil {
		authCfg.MaxBodyBytes = cfg.HTTP.MaxBodyBytes
	}
	verifier, err := gsrv.NewVerifier(authCfg)
	if err != nil {
		return nil, err
	}
	var metrics *gsrv.Metrics
	if cfg.Metrics.Enabled {
		metrics, err = gsrv.NewMetrics(cfg.Name)
		if err != nil {
			return nil, err
		}
		metrics.BindNonceStats(verifier.NonceStats)
	}
	quota, err := gsrv.NewLocalLimiter(cfg.Name+":"+rand.Text(), cfg.RateLimitQPS)
	if err != nil {
		return nil, err
	}
	state := gsrv.NewHealth()
	module, err := infort.Build(cfg.Name, cfg.Clients, metrics)
	if err != nil {
		return nil, err
	}
	assembled := false
	defer func() {
		if !assembled {
			resultErr = errors.Join(resultErr, module.Close())
		}
	}()
	public, backend, err := buildGRPCServers(cfg, state, verifier, quota, metrics, module)
	if err != nil {
		return nil, err
	}
	front, bridge, err := buildHTTPServer(cfg, state, verifier, quota, metrics)
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
	app, err := gsrv.New(cfg.Name, gsrv.WithServers(registered...), gsrv.WithHealth(state), gsrv.WithShutdownTimeout(cfg.ShutdownTimeout), gsrv.WithHook(gsrv.Hook{Name: "downstream-clients", Start: func(context.Context) error { return nil }, Stop: func(context.Context) error { return module.Close() }}))
	if err != nil {
		if bridge != nil {
			err = errors.Join(err, bridge.Close())
		}
		return nil, err
	}
	assembled = true
	return &servers{app: app, http: front, grpc: public, backend: backend, gateway: bridge, metrics: metrics, module: module}, nil
}

// Serve 托管 SIGINT/SIGTERM；HTTP→公开gRPC→内部gRPC 顺序排空。
func (s *servers) Serve(ctx context.Context) error { return s.app.RunSignals(ctx) }

// Close 在 App 退出后释放自持 gateway 连接；logger 最后 Sync。
func (s *servers) Close() error {
	var err error
	if s.gateway != nil {
		err = s.gateway.Close()
	}
	return errors.Join(err, s.module.Close())
}
