// Package config 组合框架配置和本服务配置；真实凭据仅从指定环境变量读取。
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gtkit/gsrv"
)

// Key 配置调用方身份及密钥环境变量引用，不存放明文凭据。
type Key struct {
	AppKey    string `yaml:"app_key"`    // 调用服务公开标识；必填，1–64 位字母/数字/点/下划线/短横线。
	KeyID     string `yaml:"key_id"`     // 密钥轮换标识；必填，同一 AppKey 内唯一，字符约束同 app_key。
	SecretEnv string `yaml:"secret_env"` // HMAC secret 环境变量名称；HTTP 启用时必填，变量值至少 32 字节。
	TokenEnv  string `yaml:"token_env"`  // gRPC token 环境变量名称；公开 gRPC 启用时必填，变量值至少 32 字节。
}

// Auth 配置默认内存防重放容量；仅影响 HTTP 签名 nonce，不保存凭据。
type Auth struct {
	NonceCapacity          int `yaml:"nonce_capacity"`             // 全局有效 nonce 上限；0/缺省=100000，必须为正且不少于 HTTP AppKey 数。
	NonceCapacityPerAppKey int `yaml:"nonce_capacity_per_app_key"` // 每 AppKey 预留；0/缺省=全局容量/HTTP AppKey 数，正值不可超过均分值，KeyID 轮换共用。
}

// Metrics 配置本机可选采集，不影响日志。
type Metrics struct {
	Enabled bool `yaml:"enabled"` // 是否启用指标及 GET /metrics；缺省 false；启用要求 HTTP，且只允许直连 loopback 来源。
}

// Config 按协议是否出现配置块显式启用服务，不根据环境名推断行为。
type Config struct {
	Name            string            `yaml:"name"`             // 服务名称及 HMAC 目标服务；必填，约束同 AppKey。
	ShutdownTimeout time.Duration     `yaml:"shutdown_timeout"` // 全局排空预算；缺省 20s，必须为正且小于 systemd TimeoutStopSec。
	HTTP            *gsrv.HTTPConfig  `yaml:"http"`             // HTTP 配置块；整段缺失即关闭；子字段默认值见框架 Config。
	GRPC            *gsrv.GRPCConfig  `yaml:"grpc"`             // 公开 gRPC 配置块；整段缺失即关闭；至少启用一种公开协议。
	GatewaySocket   string            `yaml:"gateway_socket"`   // proto HTTP gateway 私有 socket 路径；留空关闭，非空要求 HTTP，父目录仅服务用户可访问。
	RateLimitQPS    int               `yaml:"rate_limit_qps"`   // 每 AppKey 的 HTTP/gRPC 合计本地 QPS；缺省 0=显式不启用，负数非法；burst=QPS。
	Auth            Auth              `yaml:"auth"`             // HTTP 防重放容量；缺省采用有界默认值，无外部存储依赖。
	Metrics         Metrics           `yaml:"metrics"`          // 最小运行指标；缺省关闭，启用要求 HTTP 本机管理路由。
	Keys            []Key             `yaml:"keys"`             // 调用方凭据引用列表；必填，不能通过空列表关闭鉴权。
	Log             gsrv.LogConfig    `yaml:"log"`              // 日志输出；path 空则控制台，console 缺省 true，level 缺省 info。
	Credentials     []gsrv.Credential `yaml:"-"`                // 运行期从环境变量解析，不进入 YAML。
}

// Load 严格解析、补默认值并校验 secret 环境变量；错误不回显凭据。
func Load(path string) (*Config, error) {
	var cfg Config
	if err := gsrv.LoadConfig(path, &cfg); err != nil {
		return nil, err
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 20 * time.Second
	}
	if cfg.Auth.NonceCapacity == 0 {
		cfg.Auth.NonceCapacity = 100000
	}
	if cfg.Auth.NonceCapacity < 0 || cfg.Auth.NonceCapacityPerAppKey < 0 || cfg.Metrics.Enabled && cfg.HTTP == nil || cfg.GRPC != nil && cfg.GRPC.MaxInflight < 0 {
		return nil, errors.New("service: invalid nonce capacity, metrics protocol or gRPC inflight limit")
	}
	if !gsrv.ValidName(cfg.Name) || cfg.ShutdownTimeout <= 0 || cfg.RateLimitQPS < 0 || (cfg.HTTP == nil && cfg.GRPC == nil) || len(cfg.Keys) == 0 {
		return nil, errors.New("service: invalid name, protocols, shutdown budget, QPS or keys")
	}
	if cfg.GatewaySocket != "" {
		if cfg.HTTP == nil {
			return nil, errors.New("service: gateway requires HTTP")
		}
		socket, err := filepath.Abs(cfg.GatewaySocket)
		if err != nil {
			return nil, fmt.Errorf("gateway socket: %w", err)
		}
		cfg.GatewaySocket = socket
	}
	for _, key := range cfg.Keys {
		credential := gsrv.Credential{AppKey: key.AppKey, KeyID: key.KeyID}
		if cfg.HTTP != nil {
			value, err := gsrv.SecretFromEnv(key.SecretEnv)
			if err != nil {
				return nil, err
			}
			credential.Secret = value
		}
		if cfg.GRPC != nil {
			value, err := gsrv.SecretFromEnv(key.TokenEnv)
			if err != nil {
				return nil, err
			}
			credential.Token = value
		}
		cfg.Credentials = append(cfg.Credentials, credential)
	}
	if cfg.HTTP != nil {
		callers := make(map[string]struct{})
		for _, key := range cfg.Keys {
			callers[key.AppKey] = struct{}{}
		}
		if cfg.Auth.NonceCapacity < len(callers) || cfg.Auth.NonceCapacityPerAppKey > cfg.Auth.NonceCapacity/len(callers) {
			return nil, errors.New("service: nonce reservations exceed global capacity")
		}
	}
	return &cfg, nil
}
