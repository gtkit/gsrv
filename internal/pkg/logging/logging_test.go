package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtkit/logger/v2"

	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

func TestConsoleOnlyAndInvalidConfig(t *testing.T) {
	log, err := New(Config{})
	if err != nil {
		t.Fatalf("empty path must allow console-only startup: %v", err)
	}
	log.Sync()
	for _, cfg := range []Config{{Console: new(false)}, {Level: "unknown"}} {
		if _, err := New(cfg); err == nil {
			t.Fatal("invalid log configuration accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Path: filepath.Join(path, "access")}); err == nil {
		t.Fatal("invalid file path accepted")
	}
}

func TestProtocolCodeNormalized(t *testing.T) {
	directory := t.TempDir()
	log, err := New(Config{JSON: true, Path: filepath.Join(directory, "access"), Console: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	previous := logger.Default()
	logger.SetDefault(log)
	t.Cleanup(func() { log.Sync(); logger.SetDefault(previous) })
	scope := request.New("grpc-request")
	scope.Authenticate("caller")
	Access(request.WithScope(t.Context(), scope), "test", "grpc", "/test.Service/Call", "ResourceExhausted", 0, time.Millisecond)
	log.Sync()
	var contents strings.Builder
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		contents.Write(body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"error_code":"RESOURCE_EXHAUSTED"`, `"grpc_code":"ResourceExhausted"`, `"app_key":"caller"`, `"request_id":"grpc-request"`} {
		if !strings.Contains(contents.String(), field) {
			t.Fatalf("missing field %s", field)
		}
	}
}
