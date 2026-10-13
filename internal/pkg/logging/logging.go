// Package logging 装配 gtkit/logger/v2 并统一访问日志字段。
package logging

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	request "github.com/gtkit/gsrv/internal/pkg/requestid"
	"github.com/gtkit/logger/v2"
	"go.uber.org/zap"
)

// Config 定义日志输出；Path 空则仅控制台，Console 未设置回落 true。
type Config struct {
	_       struct{} // 强制外部使用带键字面量。
	Level   string   `yaml:"level"`   // 日志阈值；debug/info/warn/error，缺省 info。
	JSON    bool     `yaml:"json"`    // 是否输出 JSON；缺省 false，生产建议显式 true。
	Path    string   `yaml:"path"`    // 文件路径前缀；留空关闭文件，相对路径基于进程 cwd。
	Console *bool    `yaml:"console"` // 是否同时写控制台；留空 true；不可同时关闭两类输出。
}

// ConsoleEnabled 返回控制台是否开启。
func (c Config) ConsoleEnabled() bool { return c.Console == nil || *c.Console }

// FileEnabled 返回是否配置文件输出。
func (c Config) FileEnabled() bool { return c.Path != "" }

// New 构造日志实例；调用方启动期 SetDefault 一次，在资源关闭后 Sync。
func New(cfg Config) (*logger.Logger, error) {
	if cfg.Level == "" {
		cfg.Level = "info"
	}
	switch cfg.Level {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("logging: invalid level")
	}
	if !cfg.ConsoleEnabled() && !cfg.FileEnabled() {
		return nil, fmt.Errorf("logging: no output enabled")
	}
	if cfg.FileEnabled() {
		dir := filepath.Dir(cfg.Path)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("logging directory: %w", err)
		}
		probe, err := os.CreateTemp(dir, ".gsrv-log-probe-*")
		if err != nil {
			return nil, fmt.Errorf("logging writable directory: %w", err)
		}
		name := probe.Name()
		closeErr := probe.Close()
		removeErr := os.Remove(name)
		if closeErr != nil {
			return nil, fmt.Errorf("logging probe close: %w", closeErr)
		}
		if removeErr != nil {
			return nil, fmt.Errorf("logging probe remove: %w", removeErr)
		}
	}
	options := []logger.Option{logger.WithLevel(cfg.Level), logger.WithOutJSON(cfg.JSON),
		logger.WithConsole(cfg.ConsoleEnabled()), logger.WithFile(cfg.FileEnabled()),
		logger.WithRedactKeys("secret", "token", "authorization", "signature", "password"),
		logger.WithContextFields(request.LogFields)}
	if cfg.FileEnabled() {
		options = append(options, logger.WithPath(cfg.Path), logger.WithDivision("daily"))
	}
	return logger.New(options...)
}

// Access 输出已认证身份、最终协议码与总耗时；错误文案和请求体不进入访问日志。
func Access(ctx context.Context, service, protocol, operation, code string, status int, duration time.Duration) {
	errorCode := code
	if protocol == "grpc" && code != "OK" {
		var normalized strings.Builder
		for i, c := range code {
			if i > 0 && c >= 'A' && c <= 'Z' {
				normalized.WriteByte('_')
			}
			normalized.WriteRune(c)
		}
		errorCode = strings.ToUpper(normalized.String())
	}
	appKey := ""
	degraded := false
	if scope := request.FromContext(ctx); scope != nil {
		appKey = scope.AppKey()
		degraded = scope.Degraded()
	}
	fields := []zap.Field{zap.String("service", service), zap.String("protocol", protocol),
		zap.String("operation", operation), zap.String("app_key", appKey), zap.String("error_code", errorCode),
		zap.Float64("duration_ms", float64(duration.Microseconds())/1000), zap.Bool("degraded", degraded)}
	if protocol == "http" {
		fields = append(fields, zap.Int("http_status", status))
	} else {
		fields = append(fields, zap.String("grpc_code", code))
	}
	if code == "OK" {
		logger.InfoCtx(ctx, "request completed", fields...)
	} else {
		logger.WarnCtx(ctx, "request failed", fields...)
	}
}
