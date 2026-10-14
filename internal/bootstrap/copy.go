package bootstrap

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gtkit/gsrv"
	"github.com/gtkit/gsrv/internal/platform/config"
)

// CheckCopy 校验源码自模块路径与.example中的部署标识；不读取真实配置/凭据，也不监听或发请求。
// other非空时只比较用户显式指定的服务副本，不扫描其他仓库。
func CheckCopy(root, other string) error {
	current, err := inspectCopy(root)
	if err != nil {
		return err
	}
	if other == "" {
		return nil
	}
	peer, err := inspectCopy(other)
	if err != nil {
		return err
	}
	if current.name == peer.name {
		return errors.New("copy check: service names conflict in configuration examples")
	}
	for _, left := range current.listeners {
		for _, right := range peer.listeners {
			if listenersConflict(left, right) {
				return fmt.Errorf("copy check: listener conflict (%s and %s)", left, right)
			}
		}
	}
	for _, left := range current.sockets {
		if slices.Contains(peer.sockets, left) {
			return fmt.Errorf("copy check: socket path conflicts (%s)", left)
		}
	}
	return nil
}

type copyIdentity struct {
	name               string
	listeners, sockets []string
}

func inspectCopy(root string) (copyIdentity, error) {
	var identity copyIdentity
	root, err := filepath.Abs(root)
	if err != nil {
		return identity, err
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return identity, fmt.Errorf("copy check go.mod: %w", err)
	}
	moduleLine := regexp.MustCompile(`(?m)^module\s+("[^"]+"|\S+)`).FindSubmatch(data)
	if len(moduleLine) != 2 {
		return identity, errors.New("copy check: module directive missing")
	}
	module := string(moduleLine[1])
	if strings.HasPrefix(module, `"`) {
		module, err = strconv.Unquote(module)
		if err != nil {
			return identity, err
		}
	}
	fset := token.NewFileSet()
	var problems []error
	checkGo := func(path string) error {
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, item := range file.Imports {
			importPath, err := strconv.Unquote(item.Path.Value)
			if err != nil {
				return err
			}
			foreignInternal := strings.Contains(importPath, "/internal/") && !strings.HasPrefix(importPath, module+"/internal/")
			legacy := module != "github.com/gtkit/gsrv" && (importPath == "github.com/gtkit/gsrv" || strings.HasPrefix(importPath, "github.com/gtkit/gsrv/"))
			if foreignInternal || legacy {
				problems = append(problems, fmt.Errorf("copy check: stale own import at %s:%d", path, fset.Position(item.Pos()).Line))
			}
		}
		return nil
	}
	if err := checkGo(filepath.Join(root, "gsrv.go")); err != nil {
		return identity, err
	}
	for _, directory := range []string{"cmd", "internal"} {
		if err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".go") {
				return checkGo(path)
			}
			return nil
		}); err != nil {
			return identity, err
		}
	}
	goPackage := regexp.MustCompile(`(?m)^\s*option\s+go_package\s*=\s*"([^"]+)"\s*;`)
	protoCount := 0
	if err := filepath.WalkDir(filepath.Join(root, "proto"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		protoCount++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		match := goPackage.FindSubmatch(data)
		if len(match) != 2 {
			problems = append(problems, fmt.Errorf("copy check: missing go_package at %s", path))
			return nil
		}
		packagePath, _, _ := strings.Cut(string(match[1]), ";")
		rel, err := filepath.Rel(filepath.Join(root, "proto"), filepath.Dir(path))
		if err != nil {
			return err
		}
		expected := module + "/pb"
		if rel != "." {
			expected += "/" + filepath.ToSlash(rel)
		}
		if packagePath != expected {
			problems = append(problems, fmt.Errorf("copy check: go_package disagrees with module/directory at %s", path))
		}
		return nil
	}); err != nil {
		return identity, err
	}
	if protoCount == 0 {
		problems = append(problems, errors.New("copy check: no service proto found"))
	}
	cfg, err := config.InspectExample(filepath.Join(root, "config/env/env.yml.example"))
	if err != nil {
		return identity, fmt.Errorf("copy check example: %w", err)
	}
	if !gsrv.ValidName(cfg.Name) || (cfg.HTTP == nil && cfg.GRPC == nil) {
		problems = append(problems, errors.New("copy check: valid service name and protocol required in example"))
	}
	if module != "github.com/gtkit/gsrv" && cfg.Name == "example-service" {
		problems = append(problems, errors.New("copy check: copied service must replace example-service name in .example"))
	}
	identity.name = cfg.Name
	if cfg.HTTP != nil {
		addr := cfg.HTTP.Addr
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		identity.listeners = append(identity.listeners, addr)
	}
	if cfg.GRPC != nil {
		addr := cfg.GRPC.Addr
		if cfg.GRPC.Network == "unix" {
			identity.sockets = append(identity.sockets, copySocket(root, addr))
		} else {
			if addr == "" {
				addr = "127.0.0.1:9090"
			}
			identity.listeners = append(identity.listeners, addr)
		}
	}
	if cfg.GatewaySocket != "" {
		identity.sockets = append(identity.sockets, copySocket(root, cfg.GatewaySocket))
	}
	for _, addr := range identity.listeners {
		_, port, err := net.SplitHostPort(addr)
		value, parseErr := strconv.Atoi(port)
		if err != nil || parseErr != nil || value < 1 || value > 65535 {
			problems = append(problems, errors.New("copy check: example listener needs a fixed valid port"))
		}
	}
	for i, left := range identity.listeners {
		for _, right := range identity.listeners[i+1:] {
			if listenersConflict(left, right) {
				problems = append(problems, errors.New("copy check: HTTP/gRPC example listeners conflict"))
			}
		}
	}
	for i, left := range identity.sockets {
		for _, right := range identity.sockets[i+1:] {
			if left == right {
				problems = append(problems, errors.New("copy check: public/backend sockets conflict"))
			}
		}
	}
	return identity, errors.Join(problems...)
}

func copySocket(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

func listenersConflict(left, right string) bool {
	lh, lp, le := net.SplitHostPort(left)
	rh, rp, re := net.SplitHostPort(right)
	if le != nil || re != nil || lp != rp {
		return false
	}
	return strings.EqualFold(lh, rh) || lh == "" || rh == "" || lh == "0.0.0.0" || rh == "0.0.0.0" || lh == "::" || rh == "::" || loopbackPair(lh, rh)
}

func loopbackPair(left, right string) bool {
	// localhost可解析IPv4/IPv6，保守视为与任一loopback地址冲突。
	l := strings.EqualFold(left, "localhost")
	r := strings.EqualFold(right, "localhost")
	li, ri := net.ParseIP(left), net.ParseIP(right)
	return l && (r || ri != nil && ri.IsLoopback()) || r && li != nil && li.IsLoopback()
}
