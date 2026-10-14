// Package interceptor 沿用原有位置，统一 gRPC 请求身份、日志、限流、校验与恢复。
package interceptor

import (
	"context"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gtkit/logger/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	"github.com/gtkit/gsrv/internal/pkg/limiter"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

// 私有 gateway 身份仅允许通过权限受限 Unix socket 传入。
const (
	GatewayAppKey = "x-gsrv-app-key"
	GatewayKeyID  = "x-gsrv-key-id"
)

// Authenticator 认证绑定 AppKey 的 Bearer token。
type Authenticator interface {
	AuthenticateToken(context.Context, string, string) (auth.Identity, error)
}

// Config 由 server 装配，Trusted 只用于权限受限 gateway 后端。
type Config struct {
	_              struct{}
	RequestTimeout time.Duration
	Authenticator  Authenticator
	Limiter        limiter.Limiter
	Validate       func(proto.Message) error
	Trusted        bool
}

// Unary 提供 unary 请求治理；默认值由 server 构造阶段校验。
func Unary(service string, cfg Config) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		md, _ := metadata.FromIncomingContext(ctx)
		scope := request.New(first(md, "x-request-id"))
		ctx = request.WithScope(ctx, scope)
		if headerErr := grpc.SetHeader(ctx, metadata.Pairs("x-request-id", scope.ID())); headerErr != nil {
			return nil, status.Error(codes.Internal, "response metadata failed")
		}
		start := time.Now()
		probe := isHealth(info.FullMethod)
		defer func() {
			if !probe && !cfg.Trusted {
				logging.Access(ctx, service, "grpc", info.FullMethod, status.Code(err).String(), 0, time.Since(start))
			}
		}()
		defer func() {
			if recover() != nil {
				logger.ErrorCtx(ctx, "grpc panic recovered", zap.ByteString("stack", debug.Stack()))
				resp = nil
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		defer cancel()
		if !probe {
			identity, authErr := authenticate(callCtx, md, cfg)
			if authErr != nil {
				return nil, ToStatus(authErr)
			}
			scope.Authenticate(identity.AppKey)
			callCtx = auth.WithIdentity(callCtx, identity)
			if !cfg.Trusted && cfg.Limiter != nil {
				allowed, limitErr := cfg.Limiter.Allow(callCtx, identity.AppKey)
				if limitErr != nil {
					return nil, ToStatus(fault.Wrap(fault.Unavailable, "limiter unavailable", limitErr))
				}
				if !allowed {
					return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
				}
			}
		}
		if cfg.Validate != nil {
			if message, ok := req.(proto.Message); ok {
				if validationErr := cfg.Validate(message); validationErr != nil {
					return nil, status.Error(codes.InvalidArgument, "invalid request")
				}
			}
		}
		resp, err = handler(callCtx, req)
		return resp, ToStatus(err)
	}
}

// Stream 提供 stream 建连认证/限流，并在 RecvMsg 执行参数校验。
func Stream(service string, cfg Config) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx := ss.Context()
		md, _ := metadata.FromIncomingContext(ctx)
		scope := request.New(first(md, "x-request-id"))
		ctx = request.WithScope(ctx, scope)
		start := time.Now()
		probe := isHealth(info.FullMethod)
		defer func() {
			if !probe && !cfg.Trusted {
				logging.Access(ctx, service, "grpc", info.FullMethod, status.Code(err).String(), 0, time.Since(start))
			}
		}()
		defer func() {
			if recover() != nil {
				logger.ErrorCtx(ctx, "grpc stream panic recovered", zap.ByteString("stack", debug.Stack()))
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		if !probe {
			identity, authErr := authenticate(ctx, md, cfg)
			if authErr != nil {
				return ToStatus(authErr)
			}
			scope.Authenticate(identity.AppKey)
			ctx = auth.WithIdentity(ctx, identity)
			if !cfg.Trusted && cfg.Limiter != nil {
				allowed, limitErr := cfg.Limiter.Allow(ctx, identity.AppKey)
				if limitErr != nil {
					return ToStatus(fault.Wrap(fault.Unavailable, "limiter unavailable", limitErr))
				}
				if !allowed {
					return status.Error(codes.ResourceExhausted, "rate limit exceeded")
				}
			}
		}
		if err := ss.SetHeader(metadata.Pairs("x-request-id", scope.ID())); err != nil {
			return status.Error(codes.Internal, "response metadata failed")
		}
		return ToStatus(handler(srv, &validatedStream{ServerStream: ss, ctx: ctx, validate: cfg.Validate}))
	}
}

type validatedStream struct {
	grpc.ServerStream
	ctx      context.Context
	validate func(proto.Message) error
}

func (s *validatedStream) Context() context.Context { return s.ctx }
func (s *validatedStream) RecvMsg(value any) error {
	if err := s.ServerStream.RecvMsg(value); err != nil {
		return err
	}
	if s.validate != nil {
		if message, ok := value.(proto.Message); ok {
			if err := s.validate(message); err != nil {
				return status.Error(codes.InvalidArgument, "invalid request")
			}
		}
	}
	return nil
}

func authenticate(ctx context.Context, md metadata.MD, cfg Config) (auth.Identity, error) {
	if cfg.Trusted {
		appKey, keyID := first(md, GatewayAppKey), first(md, GatewayKeyID)
		if !auth.ValidName(appKey) || !auth.ValidName(keyID) {
			return auth.Identity{}, fault.New(fault.Unauthenticated, "missing gateway identity")
		}
		return auth.Identity{AppKey: appKey, KeyID: keyID}, nil
	}
	token, ok := strings.CutPrefix(first(md, "authorization"), "Bearer ")
	if !ok {
		return auth.Identity{}, fault.New(fault.Unauthenticated, "invalid or missing credentials")
	}
	return cfg.Authenticator.AuthenticateToken(ctx, first(md, "x-app-key"), token)
}
func first(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}
func isHealth(method string) bool { return strings.HasPrefix(method, "/grpc.health.v1.Health/") }
