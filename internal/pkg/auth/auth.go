// Package auth 认证服务调用方，不判断用户权限或业务授权。
package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	encryhmac "github.com/gtkit/encry/hmac"
	"github.com/gtkit/gsrv/internal/pkg/fault"
)

// 请求签名头均须单值；Timestamp 使用 Unix 秒。
const (
	AppKeyHeader     = "X-App-Key"
	KeyIDHeader      = "X-Key-Id"
	TimestampHeader  = "X-Signature-Timestamp"
	NonceHeader      = "X-Signature-Nonce"
	SignatureHeader  = "X-Signature"
	SignatureVersion = "gsrv-hmac-v1"
)

// Credential 描述一个调用方的可轮换凭据；Secret/Token 至少有一项，非空时至少 32 字节。
type Credential struct {
	_      struct{} // 强制外部使用带键字面量。
	AppKey string
	KeyID  string
	Secret string
	Token  string
}

// Identity 是认证结果，AppKey 不从未经验证的头直接构造。
type Identity struct {
	_      struct{} // 防字段顺序成为调用契约。
	AppKey string
	KeyID  string
}

// Config 配置签名校验；默认 Skew=30s、MaxBodyBytes=1MiB、NonceCapacity=100000。
type Config struct {
	_             struct{} // 强制外部使用带键字面量。
	Service       string
	Credentials   []Credential
	Skew          time.Duration
	MaxBodyBytes  int64
	NonceCapacity int
	Nonces        NonceStore
}

// Verifier 持有构造时复制的凭据，可供两种协议同时认证。
type Verifier struct {
	cfg         Config
	credentials map[string]Credential
}

// New 验证配置并创建认证器，不允许空凭据静默关闭认证。
func New(cfg Config) (*Verifier, error) {
	if cfg.Skew == 0 {
		cfg.Skew = 30 * time.Second
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.NonceCapacity == 0 {
		cfg.NonceCapacity = 100000
	}
	if !ValidName(cfg.Service) || len(cfg.Credentials) == 0 || cfg.Skew <= 0 || cfg.Skew > 5*time.Minute || cfg.MaxBodyBytes <= 0 {
		return nil, errors.New("auth: invalid service, credentials, skew or body limit")
	}
	v := &Verifier{cfg: cfg, credentials: make(map[string]Credential, len(cfg.Credentials))}
	for _, credential := range cfg.Credentials {
		if !ValidName(credential.AppKey) || !ValidName(credential.KeyID) ||
			(credential.Secret == "" && credential.Token == "") ||
			(credential.Secret != "" && len(credential.Secret) < 32) || (credential.Token != "" && len(credential.Token) < 32) {
			return nil, errors.New("auth: invalid credential identifiers or secret length")
		}
		key := credential.AppKey + ":" + credential.KeyID
		if _, ok := v.credentials[key]; ok {
			return nil, errors.New("auth: duplicate AppKey/KeyID")
		}
		v.credentials[key] = credential
	}
	if v.cfg.Nonces == nil {
		nonces, err := NewMemoryNonceStore(cfg.NonceCapacity)
		if err != nil {
			return nil, err
		}
		v.cfg.Nonces = nonces
	}
	v.cfg.Credentials = nil // 不保留调用方 slice，避免配置热变更影响认证。
	return v, nil
}

// ValidName 限制服务名、AppKey、KeyID 为 1–64 位 ASCII 字母、数字、点、下划线、短横线。
func ValidName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// AuthenticateHTTP 在 HTTP 解析前验证原始字节并恢复 body；有界请求体、固定签名编码和 nonce。
func (v *Verifier) AuthenticateHTTP(r *http.Request) (Identity, error) {
	if r == nil || r.URL == nil {
		return Identity{}, fault.New(fault.InvalidArgument, "invalid request")
	}
	if err := r.Context().Err(); err != nil {
		return Identity{}, err
	}
	rejected := fault.New(fault.Unauthenticated, "invalid or missing credentials")
	appKey, keyID := single(r.Header, AppKeyHeader), single(r.Header, KeyIDHeader)
	timestamp, nonce := single(r.Header, TimestampHeader), single(r.Header, NonceHeader)
	signature := single(r.Header, SignatureHeader)
	credential, ok := v.credentials[appKey+":"+keyID]
	if !ok || credential.Secret == "" || !ValidName(nonce) || signature == "" || timestamp == "" || r.Header.Get("X-HTTP-Method-Override") != "" || len(r.Header.Values("Content-Type")) > 1 {
		return Identity{}, rejected
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return Identity{}, rejected
	}
	at := time.Unix(seconds, 0)
	now := time.Now()
	if now.Sub(at) > v.cfg.Skew || at.Sub(now) > v.cfg.Skew {
		return Identity{}, rejected
	}
	candidate, err := base64.StdEncoding.Strict().DecodeString(signature)
	if err != nil || len(candidate) != sha256.Size || base64.StdEncoding.EncodeToString(candidate) != signature {
		return Identity{}, rejected
	}
	body, err := readBody(r, v.cfg.MaxBodyBytes)
	if err != nil {
		return Identity{}, err
	}
	payload := canonical(v.cfg.Service, appKey, keyID, r, timestamp, nonce, body)
	expected := encryhmac.Sha256([]byte(credential.Secret), payload)
	if !hmac.Equal(candidate, expected) {
		return Identity{}, rejected
	}
	used, err := v.cfg.Nonces.Use(r.Context(), v.cfg.Service+":"+appKey+":"+nonce, at.Add(v.cfg.Skew+time.Second))
	if err != nil {
		return Identity{}, fault.Wrap(fault.Unavailable, "authentication store unavailable", err)
	}
	if !used {
		return Identity{}, rejected
	}
	return Identity{AppKey: appKey, KeyID: keyID}, nil
}

// AuthenticateToken 认证绑定 AppKey 的 gRPC Bearer token；轮换凭据不改变 AppKey 配额。
func (v *Verifier) AuthenticateToken(ctx context.Context, appKey, token string) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if !ValidName(appKey) || token == "" || len(token) > 4096 {
		return Identity{}, fault.New(fault.Unauthenticated, "invalid or missing credentials")
	}
	received := sha256.Sum256([]byte(token))
	var result Identity
	for _, credential := range v.credentials {
		expected := sha256.Sum256([]byte(credential.Token))
		if credential.AppKey == appKey && credential.Token != "" && hmac.Equal(received[:], expected[:]) {
			result = Identity{AppKey: appKey, KeyID: credential.KeyID}
		}
	}
	if result.AppKey == "" {
		return Identity{}, fault.New(fault.Unauthenticated, "invalid or missing credentials")
	}
	return result, nil
}

// SignHTTP 为有界 HTTP 请求设置签名头；at/nonce 由调用方提供，重试必须使用新 nonce。
// credential.Secret 不进入任何请求头。签名服务名必须与目标服务配置一致。
func SignHTTP(r *http.Request, service string, credential Credential, at time.Time, nonce string) error {
	if r == nil || r.URL == nil || !ValidName(service) || !ValidName(credential.AppKey) || !ValidName(credential.KeyID) || !ValidName(nonce) || len(credential.Secret) < 32 || len(r.Header.Values("Content-Type")) > 1 {
		return errors.New("auth: invalid signing inputs")
	}
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	body, err := readBody(r, 1<<20)
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(at.Unix(), 10)
	signature := encryhmac.Sha256([]byte(credential.Secret), canonical(service, credential.AppKey, credential.KeyID, r, timestamp, nonce, body))
	r.Header.Set(AppKeyHeader, credential.AppKey)
	r.Header.Set(KeyIDHeader, credential.KeyID)
	r.Header.Set(TimestampHeader, timestamp)
	r.Header.Set(NonceHeader, nonce)
	r.Header.Set(SignatureHeader, base64.StdEncoding.EncodeToString(signature))
	return nil
}

func canonical(service, appKey, keyID string, r *http.Request, timestamp, nonce string, body []byte) []byte {
	digest := sha256.Sum256(body)
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return []byte(strings.Join([]string{SignatureVersion, service, appKey, keyID, r.Method, path, r.URL.RawQuery, r.Header.Get("Content-Type"), timestamp, nonce, hex.EncodeToString(digest[:])}, "\n"))
}

func single(headers http.Header, key string) string {
	values := headers.Values(key)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	closeErr := r.Body.Close()
	if err != nil || closeErr != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, fault.New(fault.PayloadTooLarge, "request body too large")
		}
		return nil, fault.Wrap(fault.InvalidArgument, "cannot read request body", errors.Join(err, closeErr))
	}
	if int64(len(body)) > limit {
		return nil, fault.New(fault.PayloadTooLarge, "request body too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

type identityKey struct{}

// WithIdentity 将认证结果绑定到当前调用上下文。
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// FromContext 获取认证结果，未认证返回 false。
func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	return identity, ok
}
