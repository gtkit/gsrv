package main

import (
	"context"
	"example.com/service/internal/bootstrap"
	"flag"
	"github.com/gtkit/logger/v2"
)

func main() {
	path := flag.String("config", "config/env/env.yml", "service configuration")
	flag.Parse()
	if err := bootstrap.Run(context.Background(), *path); err != nil {
		logger.Fatalf("service stopped: %v", err)
	}
}
