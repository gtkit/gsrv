package bootstrap

import (
	"github.com/gtkit/gsrv"
	"github.com/gtkit/logger/v2"
)

// newLogger 在原日志装配位置接入公共入口；实现位于 internal/pkg/logging。
func newLogger(cfg gsrv.LogConfig) (*logger.Logger, error) { return gsrv.NewLogger(cfg) }
