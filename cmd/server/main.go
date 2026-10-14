package main

import (
	"context"
	"flag"

	"github.com/gtkit/logger/v2"

	"github.com/gtkit/gsrv/internal/bootstrap"
)

func main() {
	path := flag.String("config", "config/env/env.yml", "service configuration")
	flag.Parse()
	if err := bootstrap.Run(context.Background(), *path); err != nil {
		logger.Fatalf("service stopped: %v", err)
	}
}
