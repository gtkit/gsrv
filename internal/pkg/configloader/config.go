// Package configloader 从单份 YAML 文档严格加载服务自定义配置。
package configloader

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/gtkit/logger/v2"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Load 解码至非 nil 指针，拒绝未知字段和额外 YAML 文档。
// 默认值与业务约束由调用方在返回后处理；不会自动读取环境变量或输出配置内容。
func Load(path string, target any) error {
	value := reflect.ValueOf(target)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("config: non-nil pointer required")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config open: %w", err)
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			logger.Error("config close", zap.Error(err))
		}
	}(f)
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("config decode: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config: expected exactly one YAML document")
		}
		return fmt.Errorf("config trailing document: %w", err)
	}
	return nil
}

// Secret 从明确指定的环境变量读取凭据；未设置或空值报错，不回显值。
func Secret(name string) (string, error) {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", fmt.Errorf("config: secret environment variable %q is missing", name)
	}
	return value, nil
}
