//go:build ignore

// Run with go generate ./cmd/gsrv. Only the service allowlist is bundled.
package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	check := flag.Bool("check", false, "compare the bundled snapshot without writing files")
	flag.Parse()
	if err := packTemplate(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func packTemplate(check bool) error {
	manifest, err := os.ReadFile("template-files.txt")
	if err != nil {
		return err
	}
	root := filepath.Join("..", "..")
	var files []string
	for line := range strings.Lines(string(manifest)) {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if !strings.HasSuffix(name, "/") {
			files = append(files, name)
			continue
		}
		if err := fs.WalkDir(os.DirFS(root), strings.TrimSuffix(name, "/"), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			if !entry.IsDir() && (strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".proto") || strings.HasSuffix(path, ".sh")) {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	slices.Sort(files)
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, name := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("template file is not regular: %s", name)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := fs.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := entry.Write(data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if check {
		current, err := os.ReadFile("template.zip")
		if err != nil {
			return err
		}
		if !bytes.Equal(current, output.Bytes()) {
			return fmt.Errorf("bundled template is stale; run make template from the repository root")
		}
		return nil
	}
	return os.WriteFile("template.zip", output.Bytes(), 0o644)
}
