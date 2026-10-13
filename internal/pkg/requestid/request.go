// Package requestid 在两种协议之间共享请求标识和认证后的访问记录字段。
package requestid

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Scope 保存请求元信息；AppKey 只由认证入口写入，外层日志可读取内层认证结果。
type Scope struct {
	id       string
	mu       sync.RWMutex
	appKey   string
	degraded bool
}

// New 创建请求记录；只接受长度不超过 128 的可打印 ASCII request ID。
func New(id string) *Scope {
	if !ValidID(id) {
		id = rand.Text()
	}
	return &Scope{id: id}
}

// ValidID 判断 request ID 能否安全进入日志和响应头。
func ValidID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool { return r < 33 || r > 126 })
}

// ID 返回不可变请求标识。
func (s *Scope) ID() string { return s.id }

// Authenticate 写入已验证 AppKey。
func (s *Scope) Authenticate(appKey string) { s.mu.Lock(); s.appKey = appKey; s.mu.Unlock() }

// AppKey 返回已验证 AppKey；未认证时为空。
func (s *Scope) AppKey() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.appKey }

// MarkDegraded 标记本请求已采用业务提供的降级结果。
func (s *Scope) MarkDegraded() { s.mu.Lock(); s.degraded = true; s.mu.Unlock() }

// Degraded 返回是否采用降级结果。
func (s *Scope) Degraded() bool { s.mu.RLock(); defer s.mu.RUnlock(); return s.degraded }

type contextKey struct{}

// WithScope 将请求记录放入 ctx。
func WithScope(ctx context.Context, scope *Scope) context.Context {
	return context.WithValue(ctx, contextKey{}, scope)
}

// FromContext 返回请求记录；非请求路径返回 nil。
func FromContext(ctx context.Context) *Scope { s, _ := ctx.Value(contextKey{}).(*Scope); return s }

// LogFields 供 logger.WithContextFields 注册，统一注入 request_id。
func LogFields(ctx context.Context) []zap.Field {
	if scope := FromContext(ctx); scope != nil {
		return []zap.Field{zap.String("request_id", scope.ID())}
	}
	return nil
}
