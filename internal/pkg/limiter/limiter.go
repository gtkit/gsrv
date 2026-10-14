// Package limiter 提供 HTTP/gRPC 共用的调用方配额入口。
package limiter

import (
	"context"
	"errors"

	"github.com/gtkit/golimit"

	"github.com/gtkit/gsrv/internal/pkg/auth"
)

// Limiter 只接受认证后的 AppKey；true 为放行，error 为无法安全判断。
type Limiter interface {
	Allow(context.Context, string) (bool, error)
}

// Local 复用 golimit v1 进程注册表；同实例同 AppKey 共享桶，burst 等于 QPS。
type Local struct {
	namespace string
	qps       int
}

// NewLocal 创建每 AppKey 本地配额；qps=0 显式关闭，负数非法；namespace 必须唯一。
func NewLocal(namespace string, qps int) (*Local, error) {
	if namespace == "" || qps < 0 {
		return nil, errors.New("limiter: namespace and nonnegative QPS required")
	}
	return &Local{namespace: namespace, qps: qps}, nil
}

// Allow 非阻塞检查配额；调用方必须先认证，未知 AppKey 不应进入本方法。
func (l *Local) Allow(ctx context.Context, appKey string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !auth.ValidName(appKey) {
		return false, errors.New("limiter: invalid authenticated AppKey")
	}
	if l.qps == 0 {
		return true, nil
	}
	return golimit.NewLimiter(l.namespace+":"+appKey, l.qps).Allow(), nil
}
