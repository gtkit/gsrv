package interceptor

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

type tokenFunc func(context.Context, string, string) (auth.Identity, error)

func (f tokenFunc) AuthenticateToken(ctx context.Context, key, token string) (auth.Identity, error) {
	return f(ctx, key, token)
}

type quotaFunc func(context.Context, string) (bool, error)

func (f quotaFunc) Allow(ctx context.Context, key string) (bool, error) { return f(ctx, key) }

type transportStream struct{ header metadata.MD }

func (*transportStream) Method() string                   { return "/test.Service/Call" }
func (s *transportStream) SetHeader(md metadata.MD) error { s.header = md; return nil }
func (*transportStream) SendHeader(metadata.MD) error     { return nil }
func (*transportStream) SetTrailer(metadata.MD) error     { return nil }

type serverStream struct {
	ctx      context.Context
	header   metadata.MD
	received int
}

func (s *serverStream) SetHeader(md metadata.MD) error { s.header = md; return nil }
func (*serverStream) SendHeader(metadata.MD) error     { return nil }
func (*serverStream) SetTrailer(metadata.MD)           {}
func (s *serverStream) Context() context.Context       { return s.ctx }
func (*serverStream) SendMsg(any) error                { return nil }
func (s *serverStream) RecvMsg(any) error              { s.received++; return nil }

func authenticatedConfig() Config {
	return Config{RequestTimeout: time.Second, Authenticator: tokenFunc(func(_ context.Context, key, token string) (auth.Identity, error) {
		if key != "caller" || token != "token" {
			return auth.Identity{}, fault.New(fault.Unauthenticated, "invalid credentials")
		}
		return auth.Identity{AppKey: key, KeyID: "key"}, nil
	})}
}

func requestContext(ctx context.Context) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs("x-app-key", "caller", "authorization", "Bearer token", "x-request-id", "grpc-request"))
}

func TestUnaryValidationRecoveryAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*Config)
		handler   grpc.UnaryHandler
		code      codes.Code
	}{
		{"identity", nil, func(ctx context.Context, _ any) (any, error) {
			identity, ok := auth.FromContext(ctx)
			if !ok || identity.AppKey != "caller" || request.FromContext(ctx).ID() != "grpc-request" {
				t.Error("request context lost")
			}
			return wrapperspb.String("ok"), nil
		}, codes.OK},
		{"validation", func(c *Config) {
			c.Validate = func(proto.Message) error { return errors.New("private validation detail") }
		}, func(context.Context, any) (any, error) { t.Error("invalid handler ran"); return nil, nil }, codes.InvalidArgument},
		{"panic", nil, func(context.Context, any) (any, error) { panic("private panic") }, codes.Internal},
		{"timeout", func(c *Config) { c.RequestTimeout = time.Millisecond }, func(ctx context.Context, _ any) (any, error) { <-ctx.Done(); return nil, ctx.Err() }, codes.DeadlineExceeded},
		{"limiter-failure", func(c *Config) {
			c.Limiter = quotaFunc(func(context.Context, string) (bool, error) { return false, errors.New("private quota failure") })
		}, func(context.Context, any) (any, error) { t.Error("failed quota handler ran"); return nil, nil }, codes.Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := authenticatedConfig()
			if test.configure != nil {
				test.configure(&cfg)
			}
			transport := &transportStream{}
			ctx := grpc.NewContextWithServerTransportStream(requestContext(t.Context()), transport)
			_, err := Unary("test", cfg)(ctx, wrapperspb.String("request"), &grpc.UnaryServerInfo{FullMethod: "/test.Service/Call"}, test.handler)
			if status.Code(err) != test.code || transport.header.Get("x-request-id")[0] != "grpc-request" {
				t.Fatalf("code=%v err=%v", status.Code(err), err)
			}
		})
	}
}

func TestStreamQuotaOnceValidationEachMessageAndRecovery(t *testing.T) {
	cfg := authenticatedConfig()
	charged, validated := 0, 0
	cfg.Limiter = quotaFunc(func(context.Context, string) (bool, error) { charged++; return true, nil })
	cfg.Validate = func(proto.Message) error { validated++; return nil }
	ss := &serverStream{ctx: requestContext(t.Context())}
	err := Stream("test", cfg)(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}, func(_ any, s grpc.ServerStream) error {
		identity, ok := auth.FromContext(s.Context())
		if !ok || identity.AppKey != "caller" {
			t.Error("stream identity lost")
		}
		for range 2 {
			if err := s.RecvMsg(wrapperspb.String("message")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || charged != 1 || validated != 2 || ss.received != 2 {
		t.Fatalf("stream err=%v charged=%d validated=%d", err, charged, validated)
	}
	err = Stream("test", cfg)(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}, func(any, grpc.ServerStream) error { panic("private") })
	if status.Code(err) != codes.Internal {
		t.Fatal("stream panic not recovered")
	}
	cfg.Validate = func(proto.Message) error { return errors.New("invalid") }
	err = Stream("test", cfg)(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}, func(_ any, s grpc.ServerStream) error { return s.RecvMsg(wrapperspb.String("invalid")) })
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal("stream message not validated")
	}
}

func TestStreamHealthExemptAndDuplicateCredentialsRejected(t *testing.T) {
	cfg := authenticatedConfig()
	called := false
	ss := &serverStream{ctx: t.Context()}
	err := Stream("test", cfg)(nil, ss, &grpc.StreamServerInfo{FullMethod: "/grpc.health.v1.Health/Watch"}, func(any, grpc.ServerStream) error { called = true; return nil })
	if err != nil || !called {
		t.Fatal("health stream must be exempt")
	}
	ss.ctx = metadata.NewIncomingContext(t.Context(), metadata.Pairs("x-app-key", "caller", "authorization", "Bearer token", "authorization", "Bearer forged"))
	called = false
	err = Stream("test", cfg)(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}, func(any, grpc.ServerStream) error { called = true; return nil })
	if status.Code(err) != codes.Unauthenticated || called {
		t.Fatal("duplicate credentials accepted")
	}
}
