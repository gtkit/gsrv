package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gtkit/httpc"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/breaker"
	"github.com/gtkit/gsrv/internal/pkg/configloader"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

// ClientConfig 是固定下游HTTP传输参数；身份、预算和共享并发由构造参数提供。
type ClientConfig struct {
	Service             string `yaml:"service"`                 // 被调用服务的HMAC目标名；必填，1–64位安全ASCII，与对方name一致。
	BaseURL             string `yaml:"base_url"`                // 固定origin，不含用户/查询/fragment/路径前缀；HTTP仅loopback，其他必须HTTPS。
	CAFile              string `yaml:"ca_file"`                 // HTTPS额外CA路径，空使用系统信任；HTTP时必须空，不关闭证书校验。
	MaxRequestBytes     int64  `yaml:"max_request_bytes"`       // 原始签名请求体上限；0/缺省=1MiB，必须在(0,1MiB]。
	MaxResponseBytes    int64  `yaml:"max_response_bytes"`      // 解压后的响应读取上限；0/缺省=10MiB，必须>0且<MaxInt64。
	MaxIdleConnsPerHost int    `yaml:"max_idle_conns_per_host"` // 每目标空闲连接；0/缺省=10，必须>0且<=max_conns_per_host。
	MaxConnsPerHost     int    `yaml:"max_conns_per_host"`      // 连接总数上限；0/缺省=100，必须>0；不能替代共享请求并发。
}

// Normalize 校验静态origin与配置默认值，不读取CA、不发请求。
func (cfg ClientConfig) Normalize() (ClientConfig, error) {
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 1 << 20
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 10 << 20
	}
	if cfg.MaxIdleConnsPerHost == 0 {
		cfg.MaxIdleConnsPerHost = 10
	}
	if cfg.MaxConnsPerHost == 0 {
		cfg.MaxConnsPerHost = 100
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.BaseURL, "#") || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") || !auth.ValidName(cfg.Service) {
		return cfg, errors.New("HTTP client: valid service and fixed http(s) origin required")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return cfg, errors.New("HTTP client: port must be between 1 and 65535")
		}
	}
	if u.Scheme == "http" && (!configloader.LoopbackHost(u.Hostname()) || cfg.CAFile != "") {
		return cfg, errors.New("HTTP client: remote origin requires TLS")
	}
	if cfg.MaxRequestBytes <= 0 || cfg.MaxRequestBytes > 1<<20 || cfg.MaxResponseBytes <= 0 || cfg.MaxResponseBytes == math.MaxInt64 || cfg.MaxIdleConnsPerHost <= 0 || cfg.MaxConnsPerHost <= 0 || cfg.MaxIdleConnsPerHost > cfg.MaxConnsPerHost {
		return cfg, errors.New("HTTP client: invalid body or connection limits")
	}
	return cfg, nil
}

// ClientResponse 保留协议结果供runtime adapter翻译；不会自动解释业务payload。
type ClientResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Client 复用一个独占连接池，与gRPC通过同一Dependency共享准入。
type Client struct {
	cfg        ClientConfig
	base       *url.URL
	credential auth.Credential
	dependency *breaker.Dependency
	http       *httpc.Client
	transport  *http.Transport
	lifetime   context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closed     bool
	active     sync.WaitGroup
	closeOnce  sync.Once
}

// NewClient 显式绑定本服务凭据；禁止重定向，不自动重试、降级或添加幂等键。
func NewClient(cfg ClientConfig, credential auth.Credential, dependency *breaker.Dependency) (*Client, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	if dependency == nil || !auth.ValidName(dependency.Name()) || !auth.ValidName(credential.AppKey) || !auth.ValidName(credential.KeyID) || len(credential.Secret) < 32 {
		return nil, errors.New("HTTP client: dependency and own HMAC credential required")
	}
	tlsConfig, err := configloader.ClientTLS(cfg.CAFile, "")
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	transport.MaxConnsPerHost = cfg.MaxConnsPerHost
	transport.MaxIdleConns = cfg.MaxIdleConnsPerHost
	lifetime, cancel := context.WithCancel(context.Background())
	base, _ := url.Parse(cfg.BaseURL) // Normalize已校验。
	client := httpc.New(httpc.WithTransport(transport), httpc.WithTimeout(dependency.Timeout()), httpc.WithMaxResponseBytes(cfg.MaxResponseBytes), httpc.WithoutRedirect())
	return &Client{cfg: cfg, base: base, credential: credential, dependency: dependency, http: client, transport: transport, lifetime: lifetime, cancel: cancel}, nil
}

// Do 对以/开头的路径（可含query）发送已编码body；headers被复制，身份字段由本服务重写。
// 非2xx返回响应与安全错误；超大响应不返回body。调用期间不得修改body输入。
func (c *Client) Do(ctx context.Context, method, path string, body []byte, headers http.Header) (result *ClientResponse, err error) {
	if c == nil || c.dependency == nil {
		return nil, fault.New(fault.InvalidArgument, "uninitialized HTTP client")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fault.New(fault.Unavailable, "HTTP client closed")
	}
	c.active.Add(1)
	c.mu.Unlock()
	defer c.active.Done()
	callCtx, finish, err := c.dependency.Begin(ctx, "http", c.credential.AppKey)
	if err != nil {
		return nil, err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = finish(fault.New(fault.Internal, "HTTP client panic")) // 清理后保留原panic。
			panic(p)
		}
		err = finish(err)
	}()
	ref, parseErr := url.Parse(path)
	if parseErr != nil || ref.IsAbs() || ref.Host != "" || ref.User != nil || ref.Opaque != "" || ref.Fragment != "" || !strings.HasPrefix(ref.Path, "/") {
		return nil, fault.New(fault.InvalidArgument, "HTTP path must stay within configured origin")
	}
	if int64(len(body)) > c.cfg.MaxRequestBytes {
		return nil, fault.New(fault.PayloadTooLarge, "HTTP request body exceeds limit")
	}
	reqCtx, cancel := context.WithCancel(callCtx)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	target := c.base.ResolveReference(ref)
	r, buildErr := http.NewRequestWithContext(reqCtx, method, target.String(), bytes.NewReader(body))
	if buildErr != nil {
		return nil, fault.New(fault.InvalidArgument, "invalid HTTP request")
	}
	r.Header = make(http.Header)
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "x-app-key" || lower == "x-key-id" || lower == "x-request-id" || strings.HasPrefix(lower, "x-signature") || strings.HasPrefix(lower, "grpc-metadata-") || strings.HasPrefix(lower, "x-gsrv-") {
			continue
		}
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	if r.Header.Get("X-HTTP-Method-Override") != "" {
		return nil, fault.New(fault.InvalidArgument, "HTTP method override is not supported")
	}
	r.Header.Set("X-Request-ID", request.FromContext(callCtx).ID())
	if err := auth.SignHTTP(r, c.cfg.Service, c.credential, time.Now(), rand.Text()); err != nil {
		return nil, fault.Wrap(fault.InvalidArgument, "HTTP signing failed", err)
	}
	r.GetBody = nil // 不重放已签名请求，避免不确定断连后复用已消费的nonce；保留连接池。
	response, err := c.http.Do(r)
	if err != nil {
		return nil, fault.Wrap(fault.Unavailable, "downstream HTTP transport failed", err)
	}
	result = &ClientResponse{StatusCode: response.StatusCode, Header: response.Header.Clone()}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, c.cfg.MaxResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return result, fault.Wrap(fault.Unavailable, "downstream HTTP response read failed", errors.Join(readErr, closeErr))
	}
	if int64(len(data)) > c.cfg.MaxResponseBytes {
		return result, fault.New(fault.PayloadTooLarge, "downstream HTTP response exceeds limit")
	}
	result.Body = data
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, fault.New(httpClientCode(response.StatusCode), "downstream HTTP status rejected")
	}
	return result, nil
}

func httpClientCode(status int) fault.Code {
	switch status {
	case http.StatusBadRequest:
		return fault.InvalidArgument
	case http.StatusUnauthorized:
		return fault.Unauthenticated
	case http.StatusForbidden:
		return fault.PermissionDenied
	case http.StatusNotFound:
		return fault.NotFound
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return fault.DeadlineExceeded
	case http.StatusRequestEntityTooLarge:
		return fault.PayloadTooLarge
	case http.StatusTooManyRequests:
		return fault.ResourceExhausted
	default:
		if status >= 400 && status < 500 {
			return fault.InvalidArgument
		}
		return fault.Unavailable
	}
}

// Close 拒绝新调用、取消本客户端在途请求，等待释放后关闭空闲连接；允许重复调用。
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
		c.active.Wait()
		if c.transport != nil {
			c.transport.CloseIdleConnections()
		}
	})
	return nil
}
