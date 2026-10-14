package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/gtkit/gsrv"
)

// normalizeClient 校验并补齐默认值；不读取凭据或建立连接。
func normalizeClient(cfg gsrv.DownstreamConfig) (gsrv.DownstreamConfig, error) {
	if !gsrv.ValidName(cfg.AppKey) || (cfg.HTTP == nil && cfg.GRPC == nil) {
		return cfg, errors.New("clients: own AppKey and at least one protocol required")
	}
	var err error
	cfg.Protection, err = cfg.Protection.Normalize()
	if err != nil {
		return cfg, err
	}
	if cfg.HTTP != nil {
		if !gsrv.ValidName(cfg.KeyID) || !validEnvReference(cfg.SecretEnv) {
			return cfg, errors.New("clients: HTTP key_id and secret_env required")
		}
		value, err := cfg.HTTP.Normalize()
		if err != nil {
			return cfg, err
		}
		cfg.HTTP = new(value)
	}
	if cfg.GRPC != nil {
		if !validEnvReference(cfg.TokenEnv) {
			return cfg, errors.New("clients: gRPC token_env required")
		}
		value, err := cfg.GRPC.Normalize()
		if err != nil {
			return cfg, err
		}
		cfg.GRPC = new(value)
	}
	return cfg, nil
}

var envReference = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvReference(name string) bool { return envReference.MatchString(name) }

// InspectExample 只校验配置结构与可选客户端默认值，不读取环境变量或连接下游。
func InspectExample(path string) (*Config, error) {
	var cfg Config
	if err := gsrv.LoadConfig(path, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Clients) > 64 {
		return nil, errors.New("clients: at most 64 dependencies allowed")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Clients)) {
		if !gsrv.ValidName(name) {
			return nil, errors.New("clients: invalid fixed dependency name")
		}
		client, err := normalizeClient(cfg.Clients[name])
		if err != nil {
			return nil, fmt.Errorf("inspect client %s: %w", name, err)
		}
		cfg.Clients[name] = client
	}
	return &cfg, nil
}

// resolveClient 在加载时解析指定环境变量并校验长度，错误不回显凭据；不发请求。
func resolveClient(cfg gsrv.DownstreamConfig) (gsrv.DownstreamConfig, error) {
	cfg, err := normalizeClient(cfg)
	if err != nil {
		return cfg, err
	}
	cfg.Credential = gsrv.Credential{AppKey: cfg.AppKey, KeyID: cfg.KeyID}
	if cfg.HTTP != nil {
		secret, err := gsrv.SecretFromEnv(cfg.SecretEnv)
		if err != nil {
			return cfg, err
		}
		if len(secret) < 32 {
			return cfg, errors.New("clients: HTTP secret must contain at least 32 bytes")
		}
		cfg.Credential.Secret = secret
	}
	if cfg.GRPC != nil {
		token, err := gsrv.SecretFromEnv(cfg.TokenEnv)
		if err != nil {
			return cfg, err
		}
		if len(token) < 32 || len(token) > 4096 {
			return cfg, errors.New("clients: gRPC token must contain 32 to 4096 bytes")
		}
		cfg.Credential.Token = token
	}
	return cfg, nil
}
