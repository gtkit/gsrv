package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleMatchesLoadedConfigAndDefaults(t *testing.T) {
	t.Setenv("SERVICE_APP_SECRET", strings.Repeat("s", 32))
	t.Setenv("SERVICE_APP_TOKEN", strings.Repeat("t", 32))
	cfg, err := Load("../../../config/env/env.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP == nil || cfg.GRPC == nil || cfg.GatewaySocket == "" || len(cfg.Credentials) != 1 || cfg.RateLimitQPS != 100 || !cfg.Log.ConsoleEnabled() || cfg.GRPC.MaxInflight != 100 || cfg.Auth.NonceCapacity != 100000 || cfg.Auth.NonceCapacityPerAppKey != 0 || cfg.Metrics.Enabled {
		t.Fatal("example assembly differs")
	}
	for _, test := range []struct {
		name, yaml string
		valid      bool
	}{
		{"grpc-only", "name: info\ngrpc: {}\nkeys:\n - app_key: caller\n   key_id: key\n   token_env: SERVICE_APP_TOKEN\n", true},
		{"no-protocol", "name: info\nkeys: []\n", false},
		{"negative-qps", "name: info\ngrpc: {}\nrate_limit_qps: -1\nkeys: [{}]\n", false},
		{"unknown", "name: info\nunknown: true\n", false},
		{"gateway-without-http", "name: info\ngrpc: {}\ngateway_socket: ./run/gw.sock\nkeys: [{}]\n", false},
		{"missing-secret", "name: info\nhttp: {}\nkeys:\n - app_key: caller\n   key_id: key\n   secret_env: SERVICE_MISSING_SECRET\n", false},
		{"metrics-without-http", "name: info\ngrpc: {}\nmetrics: {enabled: true}\nkeys: [{}]\n", false},
		{"negative-nonce", "name: info\nhttp: {}\nauth: {nonce_capacity: -1}\nkeys: [{}]\n", false},
		{"negative-reservation", "name: info\nhttp: {}\nauth: {nonce_capacity_per_app_key: -1}\nkeys: [{}]\n", false},
		{"negative-grpc-inflight", "name: info\ngrpc: {max_inflight: -1}\nkeys: [{}]\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(test.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t err=%v", test.valid, err)
			}
			if test.valid && (cfg.ShutdownTimeout.String() != "20s" || cfg.RateLimitQPS != 0 || cfg.Credentials[0].Secret != "" || cfg.Auth.NonceCapacity != 100000) {
				t.Fatal("defaults or disabled-protocol secret differ")
			}
		})
	}
}

func TestNonceConfigurationBoundaries(t *testing.T) {
	t.Setenv("SERVICE_APP_SECRET", strings.Repeat("s", 32))
	for _, tc := range []struct {
		name       string
		total, per int
		second     string
		valid      bool
	}{
		{"fair", 4, 0, "b", true}, {"explicit", 4, 1, "b", true}, {"small", 1, 0, "b", false},
		{"overcommitted", 4, 3, "b", false}, {"rotation", 1, 0, "a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			body := fmt.Sprintf("name: info\nhttp: {}\nmetrics: {enabled: true}\nauth: {nonce_capacity: %d, nonce_capacity_per_app_key: %d}\nkeys:\n - app_key: a\n   key_id: first\n   secret_env: SERVICE_APP_SECRET\n - app_key: %s\n   key_id: rotated\n   secret_env: SERVICE_APP_SECRET\n", tc.total, tc.per, tc.second)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err == nil && (!cfg.Metrics.Enabled || cfg.Auth.NonceCapacity != tc.total || cfg.Auth.NonceCapacityPerAppKey != tc.per) {
				t.Fatal("loaded fields differ")
			}
		})
	}
}
