package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/gtkit/logger/v2"

	"github.com/gtkit/gsrv/internal/bootstrap"
)

func main() {
	path := flag.String("config", "config/env/env.yml", "service configuration")
	checkCopy := flag.Bool("check-copy", false, "check own module and deployment example without starting servers")
	compareService := flag.String("compare-service", "", "optional explicit service checkout for copy conflict checks")
	flag.Parse()
	if *checkCopy {
		if err := bootstrap.CheckCopy(".", *compareService); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("✓ 整仓复制检查通过（module/import/go_package/配置示例标识）")
		return
	}
	if err := bootstrap.Run(context.Background(), *path); err != nil {
		logger.Fatalf("service stopped: %v", err)
	}
}
