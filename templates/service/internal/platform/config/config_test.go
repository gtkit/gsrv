package config

import (
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
	if cfg.HTTP == nil || cfg.GRPC == nil || cfg.GatewaySocket == "" || len(cfg.Credentials) != 1 || cfg.RateLimitQPS != 100 || !cfg.Log.ConsoleEnabled() {
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
			if test.valid && (cfg.ShutdownTimeout.String() != "20s" || cfg.RateLimitQPS != 0 || cfg.Credentials[0].Secret != "") {
				t.Fatal("defaults or disabled-protocol secret differ")
			}
		})
	}
}
