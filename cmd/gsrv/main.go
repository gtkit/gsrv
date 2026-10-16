package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cwd, err := os.Getwd()
	if err == nil {
		err = run(ctx, os.Args[1:], cwd, os.Stdout, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gsrv:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, cwd string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return printUsage(stdout)
	}
	switch args[0] {
	case "help", "-h", "--help":
		return printUsage(stdout)
	case "version", "--version":
		origin := provenance()
		_, err := fmt.Fprintf(stdout, "gsrv %s (revision %s, template %s)\n", origin.Version, origin.Revision, origin.TemplateSHA256)
		return err
	case "new":
		p, err := parseProject(args[1:], stderr)
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := checkTools(); err != nil {
			return err
		}
		if err := validateModule(ctx, p.module); err != nil {
			return err
		}
		if err := createProject(ctx, cwd, p, commandRunner(stdout, stderr)); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Created %s (module %s).\nNext: cd %s, copy config/env/env.yml.example to config/env/env.yml, set SERVICE_APP_SECRET and SERVICE_APP_TOKEN (at least 32 bytes each), then make run.\n", p.name, p.module, p.name)
		return err
	default:
		return fmt.Errorf("unknown command %q; run gsrv help", args[0])
	}
}

func printUsage(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  gsrv new <project-name> --module <module-path>
  gsrv version
  gsrv help

Creates a complete HTTP/gRPC service in a new directory under the current directory.
Project names: 1-64 lowercase letters, digits or hyphens; start with a letter,
end with a letter or digit. --module is required and has no default.
Requires Go 1.27.2+, make, buf, protoc-gen-go, protoc-gen-go-grpc,
protoc-gen-grpc-gateway and protoc-gen-openapiv2 in PATH.
Runs make gen and go mod tidy with GOWORK=off within a 5-minute total budget.
No tools are installed automatically. Existing destinations are rejected.
`)
	return err
}

type project struct {
	name, module string
}

func parseProject(args []string, stderr io.Writer) (project, error) {
	var p project
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		p.name, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("gsrv new", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&p.module, "module", "", "required Go module path for the generated service (no default)")
	if err := flags.Parse(args); err != nil {
		return p, err
	}
	if p.name == "" && flags.NArg() == 1 {
		p.name = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return p, errors.New("expected one project name; usage: gsrv new <project-name> --module <module-path>")
	}
	if !projectName.MatchString(p.name) {
		return p, errors.New("project name must be 1-64 lowercase letters, digits or hyphens, start with a letter and end with a letter or digit")
	}
	if p.module == "" || strings.TrimSpace(p.module) != p.module {
		return p, errors.New("--module is required and must be a Go module path without surrounding whitespace")
	}
	return p, nil
}
