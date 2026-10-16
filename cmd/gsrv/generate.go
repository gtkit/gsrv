package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:generate go run pack.go

//go:embed template.zip
var templateArchive []byte

const sourceModule = "github.com/gtkit/gsrv"

var projectName = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

var requiredTools = []string{"go", "make", "buf", "protoc-gen-go", "protoc-gen-go-grpc", "protoc-gen-grpc-gateway", "protoc-gen-openapiv2"}

func checkTools() error {
	var missing []string
	for _, name := range requiredTools {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required tools in PATH: %s; install them before running gsrv new", strings.Join(missing, ", "))
	}
	return nil
}

func validateModule(ctx context.Context, module string) (err error) {
	if strings.ContainsAny(module, "\r\n\x00") || module == "" {
		return errors.New("invalid module path")
	}
	dir, err := os.MkdirTemp("", "gsrv-module-check-")
	if err != nil {
		return fmt.Errorf("create module validation directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	modfile := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(modfile, []byte("module example.com/validation\n\ngo 1.27.0\n"), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "mod", "edit", "-module="+module, modfile)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.WaitDelay = 2 * time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("validate module %q: %w: %s", module, errors.Join(err, ctx.Err()), bytes.TrimSpace(output))
	}
	return nil
}

type runCommand func(context.Context, string, ...string) error

func commandRunner(stdout, stderr io.Writer) runCommand {
	return func(ctx context.Context, dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(args, " "), errors.Join(err, ctx.Err()))
		}
		return nil
	}
}

func createProject(ctx context.Context, parent string, p project, run runCommand) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("open parent directory: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if err := root.Mkdir(p.name, 0o755); err != nil {
		return fmt.Errorf("create project %s (destination must not exist): %w", p.name, err)
	}
	dir := filepath.Join(parent, p.name)
	defer func() {
		if err != nil {
			err = fmt.Errorf("generation failed in %s; inspect this new directory before retrying: %w", dir, err)
		}
	}()
	destination, err := root.OpenRoot(p.name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	if err := extractTemplate(ctx, destination, templateArchive, p); err != nil {
		return err
	}
	if err := run(ctx, dir, "make", "gen"); err != nil {
		return err
	}
	if err := run(ctx, dir, "go", "mod", "tidy"); err != nil {
		return err
	}
	data, err := json.Marshal(provenance())
	if err != nil {
		return fmt.Errorf("encode skeleton provenance: %w", err)
	}
	if err := destination.WriteFile(".gsrv-origin.json", append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write skeleton provenance: %w", err)
	}
	return nil
}

func extractTemplate(ctx context.Context, root *os.Root, archive []byte, p project) error {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("read bundled template: %w", err)
	}
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := file.Name
		if !fs.ValidPath(name) || strings.Contains(name, "\\") || !filepath.IsLocal(name) || !file.Mode().IsRegular() {
			return fmt.Errorf("unsafe template entry %q", name)
		}
		if file.UncompressedSize64 > 8<<20 {
			return fmt.Errorf("template entry %q exceeds 8 MiB", name)
		}
		data, err := readTemplateFile(file)
		if err != nil {
			return err
		}
		data, err = rewriteFile(name, data, p)
		if err != nil {
			return fmt.Errorf("rewrite %s: %w", name, err)
		}
		if err := root.MkdirAll(filepath.FromSlash(path.Dir(name)), 0o755); err != nil {
			return err
		}
		out, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.Mode().Perm()&0o755)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		_, writeErr := out.Write(data)
		if err := errors.Join(writeErr, out.Close()); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

func readTemplateFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, (8<<20)+1))
	if len(data) > 8<<20 {
		readErr = errors.New("template entry exceeds 8 MiB")
	}
	return data, errors.Join(readErr, reader.Close())
}

func rewriteFile(name string, data []byte, p project) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".go"):
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, data, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, item := range slices.Backward(file.Imports) {
			importPath, err := strconv.Unquote(item.Path.Value)
			if err != nil {
				return nil, err
			}
			if importPath == sourceModule || strings.HasPrefix(importPath, sourceModule+"/") {
				start, end := fset.Position(item.Path.Pos()).Offset, fset.Position(item.Path.End()).Offset
				replacement := strconv.Quote(p.module + strings.TrimPrefix(importPath, sourceModule))
				data = slices.Concat(data[:start], []byte(replacement), data[end:])
			}
		}
	case name == "go.mod":
		data = bytes.Replace(data, []byte("module "+sourceModule+"\n"), []byte("module "+p.module+"\n"), 1)
	case strings.HasSuffix(name, ".proto"):
		data = bytes.ReplaceAll(data, []byte(`"`+sourceModule+`/pb/`), []byte(`"`+p.module+`/pb/`))
	case name == "config/env/env.yml.example":
		data = bytes.Replace(data, []byte(`name: "example-service"`), []byte(`name: "`+p.name+`"`), 1)
	case name == "Makefile":
		before, rest, found := bytes.Cut(data, []byte("# gsrv:generator:start\n"))
		if found {
			_, after, found := bytes.Cut(rest, []byte("# gsrv:generator:end\n"))
			if !found {
				return nil, errors.New("generator maintenance block has no end marker")
			}
			data = slices.Concat(before, after)
		}
	case name == "README.md":
		data = []byte(fmt.Sprintf(projectReadme, p.name, p.module))
	case strings.HasSuffix(name, ".md"):
		data = bytes.ReplaceAll(data, []byte("`"+sourceModule+"`"), []byte("`"+p.module+"`"))
	}
	return data, nil
}

type origin struct {
	SourceModule   string `json:"source_module"`
	Version        string `json:"version"`
	Revision       string `json:"revision,omitzero"`
	Modified       bool   `json:"modified,omitzero"`
	TemplateSHA256 string `json:"template_sha256"`
}

func provenance() origin {
	o := origin{SourceModule: sourceModule, Version: "(devel)", TemplateSHA256: fmt.Sprintf("%x", sha256.Sum256(templateArchive))}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" {
			o.Version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				o.Revision = setting.Value
			case "vcs.modified":
				o.Modified = setting.Value == "true"
			}
		}
	}
	return o
}

const projectReadme = `# %s

Go module: %s. Generated from the gsrv HTTP/gRPC service skeleton.
Skeleton version, revision and snapshot SHA-256 are recorded in .gsrv-origin.json.

## Start

1. Use Go 1.27.2 or a newer 1.27 patch release.
2. Copy config/env/env.yml.example to config/env/env.yml.
3. Set SERVICE_APP_SECRET and SERVICE_APP_TOKEN in the environment, at least 32 bytes each.
4. Run GOWORK=off make run.

HTTP defaults to 127.0.0.1:8080 and gRPC to 127.0.0.1:9090. Configure distinct
ports/socket paths when running multiple services, and keep .example synchronized.
The example name is the HTTP HMAC target service name; callers must use that name.
GET /livez and /readyz are unauthenticated probes. The service API requires authentication.

## Develop

Keep the existing cmd/server, internal/bootstrap, internal/runtime/module,
internal/module, internal/platform, internal/pkg, proto and pb directory structure.
Edit proto contracts and run make gen; do not edit pb output directly.
Go, make, buf and all local plugins in buf.gen.yaml must be in PATH.
Run GOWORK=off make check and make build before delivery.
Architecture and operational details are in GSRV_DESIGN.md.
`
