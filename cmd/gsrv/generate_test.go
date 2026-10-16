package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"documented", []string{"order-service", "--module", "example.com/team/orders"}, true},
		{"flags first", []string{"--module=example.com/orders", "orders"}, true},
		{"single letter", []string{"a", "--module=example.com/a"}, true},
		{"maximum name", []string{strings.Repeat("a", 64), "--module=example.com/a"}, true},
		{"missing name", []string{"--module=example.com/a"}, false},
		{"missing module", []string{"orders"}, false},
		{"extra argument", []string{"orders", "--module=example.com/a", "extra"}, false},
		{"unknown flag", []string{"orders", "--module=example.com/a", "--force"}, false},
		{"parent traversal", []string{"../orders", "--module=example.com/a"}, false},
		{"absolute", []string{"/tmp/orders", "--module=example.com/a"}, false},
		{"nested", []string{"team/orders", "--module=example.com/a"}, false},
		{"backslash", []string{`team\orders`, "--module=example.com/a"}, false},
		{"uppercase", []string{"Orders", "--module=example.com/a"}, false},
		{"trailing hyphen", []string{"orders-", "--module=example.com/a"}, false},
		{"long name", []string{strings.Repeat("a", 65), "--module=example.com/a"}, false},
		{"module whitespace", []string{"orders", "--module= example.com/a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseProject(tc.args, io.Discard)
			if (err == nil) != tc.ok {
				t.Fatalf("parseProject error = %v; want success %v", err, tc.ok)
			}
		})
	}
}

func TestValidateModule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		module string
		ok     bool
	}{
		{"example.com/team/orders", true},
		{"example.com/team/orders/v2", true},
		{"", false},
		{"../orders", false},
		{"example.com//orders", false},
		{"https://example.com/orders", false},
		{"example.com/order service", false},
		{"example.com/a\nmodule evil", false},
	} {
		t.Run(tc.module, func(t *testing.T) {
			t.Parallel()
			err := validateModule(t.Context(), tc.module)
			if (err == nil) != tc.ok {
				t.Fatalf("validateModule error = %v; want success %v", err, tc.ok)
			}
		})
	}
}

func TestMissingToolsDoesNotCreateDestination(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	parent := t.TempDir()
	err := run(t.Context(), []string{"new", "orders", "--module=example.com/orders"}, parent, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("missing prerequisites accepted")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("destination changed: %v, %v", entries, err)
	}
}

func TestCreateProject(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	p := project{name: "order-service", module: "example.com/team/orders"}
	var commands []string
	err := createProject(t.Context(), parent, p, func(_ context.Context, dir string, args ...string) error {
		if dir != filepath.Join(parent, p.name) {
			t.Fatalf("unexpected command directory: %s", dir)
		}
		commands = append(commands, strings.Join(args, " "))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(commands, []string{"make gen", "go mod tidy"}) {
		t.Fatalf("unexpected command sequence: %v", commands)
	}
	root := filepath.Join(parent, p.name)
	for _, tc := range []struct{ file, text string }{
		{"go.mod", "module " + p.module},
		{"cmd/server/main.go", `"` + p.module + `/internal/bootstrap"`},
		{"proto/info/v1/info.proto", `"` + p.module + `/pb/info/v1;infov1"`},
		{"config/env/env.yml.example", `name: "order-service"`},
		{"internal/bootstrap/copy.go", `module != "github.com/gtkit/gsrv"`},
		{"README.md", "# order-service"},
	} {
		data, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil || !bytes.Contains(data, []byte(tc.text)) {
			t.Errorf("%s: missing expected content %q; error %v", tc.file, tc.text, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, ".gsrv-origin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source origin
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	if source.SourceModule != sourceModule || source.Version == "" || len(source.TemplateSHA256) != 64 {
		t.Fatalf("incomplete provenance: %+v", source)
	}
	data, err = os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil || bytes.Contains(data, []byte("cmd/gsrv")) || bytes.Contains(data, []byte("template-check")) {
		t.Fatalf("generated Makefile depends on CLI maintenance files: %v", err)
	}
	for _, name := range []string{".git", ".env", ".harness", ".codex", ".agents", "openspec", "bin", "run", "logs", "cmd/gsrv", "config/env/env.yml"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("unexpected local artifact %s: %v", name, err)
		}
	}
}

func TestExistingDestinationUntouched(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			destination := filepath.Join(parent, "orders")
			marker := destination
			switch kind {
			case "directory":
				if err := os.Mkdir(destination, 0o755); err != nil {
					t.Fatal(err)
				}
				marker = filepath.Join(destination, "marker")
			case "symlink":
				marker = filepath.Join(t.TempDir(), "marker")
				if err := os.Symlink(marker, destination); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := createProject(t.Context(), parent, project{name: "orders", module: "example.com/orders"}, func(context.Context, string, ...string) error {
				t.Fatal("command invoked for existing destination")
				return nil
			})
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("error = %v; want existing destination", err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "unchanged" {
				t.Fatalf("existing content changed: %q, %v", data, err)
			}
		})
	}
}

func TestGenerationFailure(t *testing.T) {
	t.Parallel()
	failed := errors.New("command failed")
	for _, tc := range []struct {
		name   string
		failAt int
	}{{"make gen", 1}, {"go mod tidy", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			calls := 0
			err := createProject(t.Context(), parent, project{name: "orders", module: "example.com/orders"}, func(context.Context, string, ...string) error {
				calls++
				if calls == tc.failAt {
					return failed
				}
				return nil
			})
			if !errors.Is(err, failed) || calls != tc.failAt {
				t.Fatalf("failure lost or later command executed: %v, calls %d", err, calls)
			}
			if _, err := os.Stat(filepath.Join(parent, "orders", "go.mod")); err != nil {
				t.Fatalf("partial project unavailable for inspection: %v", err)
			}
			if _, err := os.Stat(filepath.Join(parent, "orders", ".gsrv-origin.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("success marker written after failure: %v", err)
			}
		})
	}
}

func TestCancellationBeforeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	parent := t.TempDir()
	err := createProject(ctx, parent, project{name: "orders", module: "example.com/orders"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "orders")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("directory created after cancellation: %v", err)
	}
}

func TestUnsafeArchiveEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mode fs.FileMode
	}{
		{"../escape", 0o644},
		{"/absolute", 0o644},
		{`dir\escape`, 0o644},
		{"dir/../../escape", 0o644},
		{"link", fs.ModeSymlink | 0o777},
		{"pipe", fs.ModeNamedPipe | 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var data bytes.Buffer
			writer := zip.NewWriter(&data)
			header := &zip.FileHeader{Name: tc.name, Method: zip.Store}
			header.SetMode(tc.mode)
			entry, err := writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write([]byte("unsafe")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := extractTemplate(t.Context(), root, data.Bytes(), project{name: "orders", module: "example.com/orders"}); err == nil {
				t.Fatal("unsafe archive entry accepted")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unsafe archive wrote files: %v, %v", entries, err)
			}
		})
	}
}

func TestRewriteOnlyOwnImports(t *testing.T) {
	t.Parallel()
	input := []byte("package main\nimport (\n alias \"github.com/gtkit/gsrv/internal/pkg/auth\"\n \"github.com/gtkit/gsrv\"\n \"github.com/gtkit/gsrv-extra\"\n)\nconst legacy = \"github.com/gtkit/gsrv\"\n")
	output, err := rewriteFile("fixture.go", input, project{name: "orders", module: "example.com/orders"})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`alias "example.com/orders/internal/pkg/auth"`, `"example.com/orders"`, `"github.com/gtkit/gsrv-extra"`, `const legacy = "github.com/gtkit/gsrv"`} {
		if !bytes.Contains(output, []byte(text)) {
			t.Errorf("rewritten source lost %q", text)
		}
	}
}

func TestRewritePreservesServiceTargets(t *testing.T) {
	t.Parallel()
	input := []byte("build:\n\tgo build ./cmd/server\n# gsrv:generator:start\ntemplate-check:\n\tgo test ./cmd/gsrv\n# gsrv:generator:end\ncustom:\n\tgo version\n")
	output, err := rewriteFile("Makefile", input, project{name: "orders", module: "example.com/orders"})
	if err != nil {
		t.Fatal(err)
	}
	want := "build:\n\tgo build ./cmd/server\ncustom:\n\tgo version\n"
	if string(output) != want {
		t.Fatalf("service targets were changed: %q", output)
	}
}

func TestTemplateSnapshot(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "run", "pack.go", "-check")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("template consistency: %v\n%s", err, output)
	}
}

func TestCommandRunner(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	run := commandRunner(&output, &output)
	if err := run(t.Context(), t.TempDir(), "go", "env", "GOWORK"); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "off" {
		t.Fatalf("workspace not disabled: %q", output.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := run(ctx, t.TempDir(), "go", "version"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	err := run(t.Context(), t.TempDir(), "go", "gsrv-nonexistent-command")
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("command failure lost: %v", err)
	}
}

func TestHelpAndUnknownCommand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"new", "--help"}, {"version"}} {
		if err := run(t.Context(), args, t.TempDir(), io.Discard, io.Discard); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := run(t.Context(), []string{"unknown"}, t.TempDir(), io.Discard, io.Discard); err == nil {
		t.Fatal("unknown command accepted")
	}
}
