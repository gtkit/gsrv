// Package application 提供模板的只读服务信息示例，不包含中台业务规则。
package application

import "context"

// Service 保存本进程已校验的服务名称。
type Service struct{ name string }

// Result 是协议无关的服务信息。
type Result struct{ ServiceName string }

// New 构造服务信息查询。
func New(name string) *Service { return &Service{name: name} }

// Info 返回实际服务名称，并遵守请求取消。
func (s *Service) Info(ctx context.Context) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{ServiceName: s.name}, nil
}
