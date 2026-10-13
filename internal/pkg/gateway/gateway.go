// Package gateway 接入 proto 生成的 HTTP/JSON API，通过权限受限 Unix socket 调用内部 gRPC。
// Handler 必须放在公共 HTTP 治理链之后；内部身份 metadata 只从已认证 ctx 生成。
package gateway

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	grpctransport "github.com/gtkit/gsrv/internal/pkg/grpcserver"
	httptransport "github.com/gtkit/gsrv/internal/pkg/httpserver"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Register 是 proto 生成的 Register<Service>Handler 的函数签名。
type Register func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error

// Gateway 持有进程私有客户端连接；Close 应在 HTTP/gRPC 关闭后调用。
type Gateway struct {
	mux        *runtime.ServeMux
	connection *grpc.ClientConn
}

// New 使用绝对 Unix socket 路径；构造不发请求，连接由调用方负责 Close。
func New(socket string, register Register) (*Gateway, error) {
	if !filepath.IsAbs(socket) || register == nil {
		return nil, errors.New("gateway: absolute unix socket and registration required")
	}
	connection, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{EmitUnpopulated: true}),
		runtime.WithIncomingHeaderMatcher(func(string) (string, bool) { return "", false }),
		runtime.WithMetadata(func(ctx context.Context, _ *http.Request) metadata.MD {
			identity, ok := auth.FromContext(ctx)
			if !ok {
				return nil
			}
			md := metadata.Pairs(grpctransport.GatewayAppKey, identity.AppKey, grpctransport.GatewayKeyID, identity.KeyID)
			if scope := request.FromContext(ctx); scope != nil {
				md.Set("x-request-id", scope.ID())
			}
			return md
		}),
		runtime.WithOutgoingHeaderMatcher(func(key string) (string, bool) {
			if key == "x-request-id" {
				return "X-Request-Id", true
			}
			return "", false
		}),
		runtime.WithErrorHandler(func(_ context.Context, _ *runtime.ServeMux, _ runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
			code := fault.Internal
			switch status.Code(err) {
			case codes.InvalidArgument:
				code = fault.InvalidArgument
			case codes.Unauthenticated:
				code = fault.Unauthenticated
			case codes.PermissionDenied:
				code = fault.PermissionDenied
			case codes.ResourceExhausted:
				code = fault.ResourceExhausted
			case codes.Unavailable:
				code = fault.Unavailable
			case codes.DeadlineExceeded:
				code = fault.DeadlineExceeded
			case codes.Canceled:
				code = fault.Canceled
			case codes.NotFound:
				code = fault.NotFound
			}
			httptransport.WriteError(w, r, fault.New(code, "request failed"))
		}),
	)
	if err := register(context.Background(), mux, connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return &Gateway{mux: mux, connection: connection}, nil
}

// ServeHTTP 只接受认证后的上下文，客户端自带的内部身份头不会进入 metadata。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		httptransport.WriteError(w, r, fault.New(fault.Unauthenticated, "missing authenticated identity"))
		return
	}
	g.mux.ServeHTTP(w, r)
}

// Close 释放本 gateway 自持连接。
func (g *Gateway) Close() error { return g.connection.Close() }
