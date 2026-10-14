// Package bootstrap 沿用原项目分工：app 编排、logging 初始化、grpc/gateway 装配、serve 管运行。
package bootstrap

import (
	"context"
	"fmt"

	"github.com/gtkit/logger/v2"

	"github.com/gtkit/gsrv/internal/platform/config"
)

// Run 按配置→日志→服务装配→运行顺序执行，错误路径同样释放自持资源。
func Run(ctx context.Context, path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	l, err := newLogger(cfg.Log)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	logger.SetDefault(l)
	defer logger.Sync()
	srvs, err := buildServers(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := srvs.Close(); err != nil {
			logger.Warnw("close servers", "error", err)
		}
	}()
	return srvs.Serve(ctx)
}
