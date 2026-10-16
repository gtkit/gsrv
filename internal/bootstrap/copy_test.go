package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyFixture(t *testing.T, module, name string, port int) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                     "module " + module + "\n\ngo 1.27.0\n",
		"gsrv.go":                    "package gsrv\n",
		"cmd/server/main.go":         "package main\nimport \"" + module + "/internal/bootstrap\"\n",
		"internal/bootstrap/app.go":  "package bootstrap\n",
		"proto/info/v1/info.proto":   "syntax = \"proto3\";\npackage info.v1;\noption go_package = \"" + module + "/pb/info/v1;infov1\";\n",
		"config/env/env.yml.example": fmt.Sprintf("name: %s\nhttp: {addr: '127.0.0.1:%d'}\ngrpc: {addr: '127.0.0.1:%d'}\ngateway_socket: ./run/gateway.sock\nkeys:\n - app_key: caller\n   key_id: primary\n   secret_env: MUST_NOT_READ_SECRET\n   token_env: MUST_NOT_READ_TOKEN\n", name, port, port+1),
	}
	for rel, data := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCopyCheckModuleAndConflictsWithoutCredentials(t *testing.T) {
	root := copyFixture(t, "example.com/copied-service", "copied-service", 8080)
	if err := CheckCopy(root, ""); err != nil {
		t.Fatal(err)
	}
	other := copyFixture(t, "example.com/other-service", "other-service", 8082)
	if err := CheckCopy(root, other); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, module, service string
		port                  int
	}{{"service-name", "example.com/third", "copied-service", 8090}, {"listener", "example.com/third", "third", 8080}, {"legacy-name", "example.com/third", "example-service", 8090}} {
		t.Run(tc.name, func(t *testing.T) {
			peer := copyFixture(t, tc.module, tc.service, tc.port)
			if err := CheckCopy(root, peer); err == nil {
				t.Fatal("copy conflict accepted")
			}
		})
	}
	for _, tc := range []struct{ name, path, data string }{
		{"old-import", "cmd/server/main.go", "package main\nimport \"github.com/gtkit/gsrv\"\n"},
		{"foreign-internal", "cmd/server/main.go", "package main\nimport \"example.com/old/internal/bootstrap\"\n"},
		{"old-go-package", "proto/info/v1/info.proto", "syntax = \"proto3\";\noption go_package = \"example.com/old/pb/info/v1;infov1\";\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := copyFixture(t, "example.com/valid", "valid", 8090)
			if err := os.WriteFile(filepath.Join(peer, tc.path), []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := CheckCopy(peer, ""); err == nil {
				t.Fatal("stale module accepted")
			}
		})
	}
}

func TestCopyCheckOwnModuleUnderSkeletonPath(t *testing.T) {
	root := copyFixture(t, "github.com/gtkit/gsrv/order-service", "order-service", 8080)
	if err := CheckCopy(root, ""); err != nil {
		t.Fatalf("valid nested self-module rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd/server/main.go"), []byte("package main\nimport \"github.com/gtkit/gsrv\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckCopy(root, ""); err == nil {
		t.Fatal("legacy skeleton import accepted in nested module")
	}
}

func TestCopyCheckWildcardAndSocketConflicts(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		conflict    bool
	}{{"0.0.0.0:8080", "127.0.0.1:8080", true}, {"localhost:8080", "127.0.0.1:8080", true}, {"[::]:8080", "127.0.0.1:8080", true}, {"127.0.0.1:8080", "127.0.0.1:8081", false}, {"127.0.0.1:8080", "127.0.0.2:8080", false}, {"127.0.0.1:08080", "127.0.0.1:8080", true}, {"[::1]:8080", "[0:0:0:0:0:0:0:1]:8080", true}, {"[::ffff:127.0.0.1]:8080", "127.0.0.1:8080", true}} {
		if listenersConflict(tc.left, tc.right) != tc.conflict {
			t.Fatalf("conflict differs for %s/%s", tc.left, tc.right)
		}
	}
	root := copyFixture(t, "example.com/first", "first", 8080)
	peer := copyFixture(t, "example.com/second", "second", 8090)
	for _, dir := range []string{root, peer} {
		path := filepath.Join(dir, "config/env/env.yml.example")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Replace(string(data), "./run/gateway.sock", "/run/shared/gateway.sock", 1)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckCopy(root, peer); err == nil {
		t.Fatal("shared absolute socket accepted")
	}
}
