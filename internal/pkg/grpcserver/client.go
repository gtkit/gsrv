package grpcserver

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/breaker"
	"github.com/gtkit/gsrv/internal/pkg/configloader"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

// ClientConfig 是公开gRPC静态目标；不沿用私有gateway的信任通道。
type ClientConfig struct {
	Target       string `yaml:"target"`         // 必填静态host:port；不接入registry/resolver service config。
	Plaintext    bool   `yaml:"plaintext"`      // 缺省false使用TLS；true仅允许loopback/localhost，且CA/server_name必须空。
	CAFile       string `yaml:"ca_file"`        // 额外服务器CA路径；空使用系统信任，不关闭证书校验。
	ServerName   string `yaml:"server_name"`    // TLS验证/SNI名称；空使用target的host，plaintext时不可设置。
	MaxRecvBytes int    `yaml:"max_recv_bytes"` // 单条响应消息上限；0/缺省=1MiB，必须>0。
	MaxSendBytes int    `yaml:"max_send_bytes"` // 单条请求消息上限；0/缺省=1MiB，必须>0。
}

// Normalize 校验目标、TLS约束及默认值，不发起连接。
func (cfg ClientConfig) Normalize() (ClientConfig, error) {
	if cfg.MaxRecvBytes == 0 {
		cfg.MaxRecvBytes = 1 << 20
	}
	if cfg.MaxSendBytes == 0 {
		cfg.MaxSendBytes = 1 << 20
	}
	host, port, err := net.SplitHostPort(cfg.Target)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || strings.ContainsAny(host, "/@?#\\ \t\r\n") || portErr != nil || number < 1 || number > 65535 || cfg.MaxRecvBytes <= 0 || cfg.MaxSendBytes <= 0 {
		return cfg, errors.New("gRPC client: static host:port and positive message limits required")
	}
	if cfg.Plaintext && (!configloader.LoopbackHost(host) || cfg.CAFile != "" || cfg.ServerName != "") {
		return cfg, errors.New("gRPC client: plaintext is restricted to loopback without TLS options")
	}
	return cfg, nil
}

// Client 可直接供生成的New<Service>Client使用，统一控制身份、预算和全stream准入。
type Client struct {
	connection *grpc.ClientConn
	credential auth.Credential
	dependency *breaker.Dependency
	limits     []grpc.CallOption
	lifetime   context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closed     bool
	active     sync.WaitGroup
	watchers   sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

var _ grpc.ClientConnInterface = (*Client)(nil)

// NewClient 复用静态连接并禁用策略重试与远端service config；不提供默认业务重试。
// grpc-go仍可在RPC未发送/未被服务端处理时透明重试，沿用同一调用预算。
func NewClient(cfg ClientConfig, credential auth.Credential, dependency *breaker.Dependency) (*Client, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	if dependency == nil || !auth.ValidName(dependency.Name()) || !auth.ValidName(credential.AppKey) || len(credential.Token) < 32 || len(credential.Token) > 4096 {
		return nil, errors.New("gRPC client: dependency and own Bearer credential required")
	}
	var transport credentials.TransportCredentials
	if cfg.Plaintext {
		transport = insecure.NewCredentials()
	} else {
		host, _, _ := net.SplitHostPort(cfg.Target)
		name := cfg.ServerName
		if name == "" {
			name = host
		}
		tlsConfig, err := configloader.ClientTLS(cfg.CAFile, name)
		if err != nil {
			return nil, err
		}
		transport = credentials.NewTLS(tlsConfig)
	}
	conn, err := grpc.NewClient("passthrough:///"+cfg.Target, grpc.WithTransportCredentials(transport), grpc.WithDisableRetry(), grpc.WithDisableServiceConfig())
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Client{connection: conn, credential: credential, dependency: dependency, limits: []grpc.CallOption{grpc.MaxCallRecvMsgSize(cfg.MaxRecvBytes), grpc.MaxCallSendMsgSize(cfg.MaxSendBytes)}, lifetime: lifetime, cancel: cancel}, nil
}

func (c *Client) enter() error {
	if c == nil || c.connection == nil || c.dependency == nil {
		return fault.New(fault.InvalidArgument, "uninitialized gRPC client")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fault.New(fault.Unavailable, "gRPC client closed")
	}
	c.active.Add(1)
	return nil
}

func (c *Client) metadata(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	for key := range md {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "x-app-key" || lower == "x-key-id" || lower == "x-request-id" || strings.HasPrefix(lower, "x-gsrv-") || strings.HasPrefix(lower, "x-signature") {
			delete(md, key)
		}
	}
	md.Set("authorization", "Bearer "+c.credential.Token)
	md.Set("x-app-key", c.credential.AppKey)
	md.Set("x-request-id", request.FromContext(ctx).ID())
	return metadata.NewOutgoingContext(ctx, md)
}

// Invoke 兼容生成unary客户端，协议状态保留但下游错误文案不直接外泄。
func (c *Client) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) (err error) {
	if err := c.enter(); err != nil {
		return clientStatus(err)
	}
	defer c.active.Done()
	callCtx, finish, err := c.dependency.Begin(ctx, "grpc", c.credential.AppKey)
	if err != nil {
		return clientStatus(err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = finish(fault.New(fault.Internal, "gRPC client panic")) // 清理后保留原panic。
			panic(p)
		}
		err = clientStatus(finish(clientFault(err)))
	}()
	opts = append(slices.Clone(opts), c.limits...) // 配置上限不能被单次调用选项放宽，也不修改caller切片。
	err = c.connection.Invoke(c.metadata(callCtx), method, args, reply, opts...)
	return err
}

// NewStream 在整个stream生命周期占用槽位；caller必须Recv到结束或取消，预算到期自动释放。
// 客户端流的单个成功最终响应即为结束，无需再次Recv EOF。
// 发送侧EOF/CloseSend不代表完整调用成功，需继续Recv最终状态。
func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (_ grpc.ClientStream, err error) {
	if err := c.enter(); err != nil {
		return nil, clientStatus(err)
	}
	owned := true
	defer func() {
		if owned {
			c.active.Done()
		}
	}()
	callCtx, finish, err := c.dependency.Begin(ctx, "grpc", c.credential.AppKey)
	if err != nil {
		return nil, clientStatus(err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = finish(fault.New(fault.Internal, "gRPC stream panic")) // 清理后保留原panic。
			panic(p)
		}
	}()
	opts = append(slices.Clone(opts), c.limits...)
	stream, err := c.connection.NewStream(c.metadata(callCtx), desc, method, opts...)
	if err != nil {
		return nil, clientStatus(finish(clientFault(err)))
	}
	managed := &clientStream{ClientStream: stream, serverStreams: desc.ServerStreams, done: make(chan struct{})}
	var once sync.Once
	var final error
	managed.finish = func(err error) error {
		once.Do(func() {
			defer c.active.Done()
			defer close(managed.done)
			final = finish(clientFault(err))
		})
		return final
	}
	c.watchers.Go(func() {
		select {
		case <-callCtx.Done():
			_ = managed.finish(callCtx.Err()) // 无调用栈返回值，由Recv提供协议状态。
		case <-c.lifetime.Done():
			_ = managed.finish(context.Canceled)
		case <-managed.done:
		}
	})
	owned = false
	return managed, nil
}

type clientStream struct {
	grpc.ClientStream
	serverStreams bool
	done          chan struct{}
	finish        func(error) error
}

func (s *clientStream) Header() (metadata.MD, error) {
	header, err := s.ClientStream.Header()
	if err != nil {
		return header, clientStatus(s.finish(err))
	}
	return header, nil
}

func (s *clientStream) RecvMsg(message any) error {
	err := s.ClientStream.RecvMsg(message)
	if err == nil {
		if !s.serverStreams {
			return clientStatus(s.finish(nil))
		}
		return nil
	}
	if errors.Is(err, io.EOF) {
		if final := s.finish(nil); final != nil {
			return clientStatus(final)
		}
		return io.EOF
	}
	return clientStatus(s.finish(err))
}

func (s *clientStream) SendMsg(message any) error {
	err := s.ClientStream.SendMsg(message)
	if err != nil && !errors.Is(err, io.EOF) {
		return clientStatus(s.finish(err))
	}
	return err
}

func (s *clientStream) CloseSend() error {
	err := s.ClientStream.CloseSend()
	if err != nil && !errors.Is(err, io.EOF) {
		return clientStatus(s.finish(err))
	}
	return err
}

func clientFault(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	code := fault.Internal
	switch status.Code(err) {
	case codes.Canceled:
		code = fault.Canceled
	case codes.DeadlineExceeded:
		code = fault.DeadlineExceeded
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange, codes.AlreadyExists:
		code = fault.InvalidArgument
	case codes.NotFound:
		code = fault.NotFound
	case codes.Unauthenticated:
		code = fault.Unauthenticated
	case codes.PermissionDenied:
		code = fault.PermissionDenied
	case codes.ResourceExhausted:
		code = fault.ResourceExhausted
	case codes.Unavailable:
		code = fault.Unavailable
	}
	return fault.Wrap(code, "downstream RPC failed", err)
}

func clientStatus(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	if st, ok := status.FromError(err); ok {
		return status.Error(st.Code(), "downstream RPC failed")
	}
	return ToStatus(err)
}

// Close 拒绝新调用、关闭连接并取消stream，等待调用和本客户端watcher退出；可重复调用。
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		if c.cancel != nil {
			c.cancel()
		}
		if c.connection != nil {
			c.closeErr = c.connection.Close()
		}
		c.active.Wait()
		c.watchers.Wait()
	})
	return c.closeErr
}
