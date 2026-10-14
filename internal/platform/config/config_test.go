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
	if cfg.HTTP == nil || cfg.GRPC == nil || cfg.GatewaySocket == "" || len(cfg.Credentials) != 1 || cfg.RateLimitQPS != 100 || !cfg.Log.ConsoleEnabled() || cfg.GRPC.MaxInflight != 100 || cfg.Auth.NonceCapacity != 100000 || cfg.Auth.NonceCapacityPerAppKey != 0 || cfg.Metrics.Enabled || len(cfg.Clients) != 0 {
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

func TestDownstreamConfigurationDefaultsAndValidation(t *testing.T) {
	t.Setenv("SERVICE_APP_TOKEN", strings.Repeat("i", 32))
	t.Setenv("PEER_SECRET", strings.Repeat("s", 32))
	t.Setenv("PEER_TOKEN", strings.Repeat("t", 32))
	base := "name: info\ngrpc: {}\nkeys:\n - app_key: inbound\n   key_id: primary\n   token_env: SERVICE_APP_TOKEN\nclients:\n"
	for _, tc := range []struct {
		name, client string
		valid        bool
	}{
		{"both", " peer:\n  app_key: self\n  key_id: primary\n  secret_env: PEER_SECRET\n  token_env: PEER_TOKEN\n  http: {service: peer-service, base_url: 'http://127.0.0.1:8081'}\n  grpc: {target: '127.0.0.1:9091', plaintext: true}\n", true},
		{"grpc-only", " peer:\n  app_key: self\n  token_env: PEER_TOKEN\n  grpc: {target: 'example.com:443'}\n", true},
		{"missing-protocol", " peer: {app_key: self}\n", false},
		{"remote-plaintext", " peer:\n  app_key: self\n  token_env: PEER_TOKEN\n  grpc: {target: 'example.com:9091', plaintext: true}\n", false},
		{"negative-protection", " peer:\n  app_key: self\n  token_env: PEER_TOKEN\n  protection: {max_inflight: -1}\n  grpc: {target: '127.0.0.1:9091', plaintext: true}\n", false},
		{"unknown-retry", " peer:\n  app_key: self\n  token_env: PEER_TOKEN\n  retry: true\n  grpc: {target: '127.0.0.1:9091', plaintext: true}\n", false},
		{"missing-credential", " peer:\n  app_key: self\n  token_env: PEER_MISSING\n  grpc: {target: '127.0.0.1:9091', plaintext: true}\n", false},
		{"unsafe-name", " 'bad name': {app_key: self}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(base+tc.client), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err == nil {
				peer := cfg.Clients["peer"]
				if peer.Protection.Timeout.String() != "3s" || peer.Protection.MaxInflight != 100 || peer.Protection.CircuitBreaker || peer.Credential.AppKey != "self" || peer.Credential.Token != strings.Repeat("t", 32) {
					t.Fatal("defaults/own credential differ")
				}
				if peer.HTTP != nil && (peer.HTTP.MaxRequestBytes != 1<<20 || peer.HTTP.MaxResponseBytes != 10<<20 || peer.Credential.Secret != strings.Repeat("s", 32)) {
					t.Fatal("HTTP defaults differ")
				}
			}
		})
	}
}

func TestDownstreamTokenLengthMatchesAuthenticator(t *testing.T) {
	t.Setenv("SERVICE_APP_TOKEN", strings.Repeat("i", 32))
	for _, size := range []int{31, 32, 4096, 4097} {
		t.Setenv("PEER_TOKEN_SIZE", strings.Repeat("t", size))
		path := filepath.Join(t.TempDir(), "config.yml")
		data := "name: info\ngrpc: {}\nkeys:\n - app_key: inbound\n   key_id: primary\n   token_env: SERVICE_APP_TOKEN\nclients:\n peer:\n  app_key: self\n  token_env: PEER_TOKEN_SIZE\n  grpc: {target: '127.0.0.1:9091', plaintext: true}\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		valid := size >= 32 && size <= 4096
		if (err == nil) != valid {
			t.Fatalf("token length=%d valid=%t error=%v", size, valid, err)
		}
		if err != nil && strings.Contains(err.Error(), strings.Repeat("t", size)) {
			t.Fatal("token leaked into configuration error")
		}
		if valid && len(cfg.Clients["peer"].Credential.Token) != size {
			t.Fatal("credential changed during loading")
		}
	}
}

func TestInboundTokenLengthMatchesAuthenticator(t *testing.T) {
	for _, size := range []int{31, 32, 4096, 4097} {
		t.Setenv("SERVICE_APP_TOKEN", strings.Repeat("i", size))
		path := filepath.Join(t.TempDir(), "config.yml")
		data := "name: info\ngrpc: {}\nkeys:\n - app_key: inbound\n   key_id: primary\n   token_env: SERVICE_APP_TOKEN\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		valid := size >= 32 && size <= 4096
		if (err == nil) != valid {
			t.Fatalf("inbound token length=%d valid=%t error=%v", size, valid, err)
		}
		if valid && len(cfg.Credentials[0].Token) != size {
			t.Fatal("inbound credential changed during loading")
		}
	}
}

func TestCommentedClientExampleLoadsAllFields(t *testing.T) {
	t.Setenv("SERVICE_APP_SECRET", strings.Repeat("i", 32))
	t.Setenv("SERVICE_APP_TOKEN", strings.Repeat("i", 32))
	t.Setenv("PEER_APP_SECRET", strings.Repeat("s", 32))
	t.Setenv("PEER_APP_TOKEN", strings.Repeat("t", 32))
	data, err := os.ReadFile("../../../config/env/env.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	collect := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "# clients:" {
			collect = true
		}
		if collect && strings.HasPrefix(line, "# 不提供retry") {
			break
		}
		if collect {
			if !strings.HasPrefix(line, "# ") {
				t.Fatal("client example block malformed")
			}
			lines = append(lines, strings.TrimPrefix(line, "# "))
		}
	}
	if len(lines) == 0 {
		t.Fatal("client example missing")
	}
	actual := strings.Split(string(data), "\n")
	for i, line := range actual {
		if strings.HasPrefix(line, "clients: {}") {
			actual[i] = strings.Join(lines, "\n")
			break
		}
	}
	path := filepath.Join(t.TempDir(), "example.yml")
	if err := os.WriteFile(path, []byte(strings.Join(actual, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	client := cfg.Clients["peer"]
	if len(cfg.Clients) != 1 || client.HTTP == nil || client.GRPC == nil || client.HTTP.Service != "peer-service" || client.HTTP.MaxRequestBytes != 1<<20 || client.HTTP.MaxResponseBytes != 10<<20 || client.HTTP.MaxIdleConnsPerHost != 10 || client.HTTP.MaxConnsPerHost != 100 || client.GRPC.MaxRecvBytes != 1<<20 || client.GRPC.MaxSendBytes != 1<<20 || !client.GRPC.Plaintext || client.Protection.Timeout.String() != "3s" || client.Protection.MaxInflight != 100 || client.Protection.CircuitBreaker || client.Credential.AppKey != "this-service" || client.Credential.Secret != strings.Repeat("s", 32) || client.Credential.Token != strings.Repeat("t", 32) {
		t.Fatal("commented example and actual schema/defaults differ")
	}
	if _, err := InspectExample(path); err != nil {
		t.Fatal(err)
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
