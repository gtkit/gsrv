// Package info 是示例模块显式装配入口，后续仓储和 provider 在此注入。
package info

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/gtkit/gsrv"
	"github.com/gtkit/gsrv/internal/module/info/application"
)

// Components 保留模块装配结果；InfoService不调用下游，真实模块将客户端适配为自身port。
type Components struct {
	Service     *application.Service
	HTTPClients map[string]*gsrv.ServiceHTTPClient
	GRPCClients map[string]*gsrv.ServiceGRPCClient
	closeOnce   sync.Once
	closeErr    error
}

// Build 在原runtime层装配可选出站客户端；不发业务请求，失败时关闭已构造资源。
func Build(name string, clients map[string]gsrv.DownstreamConfig, metrics *gsrv.Metrics) (_ *Components, err error) {
	c := &Components{Service: application.New(name), HTTPClients: make(map[string]*gsrv.ServiceHTTPClient), GRPCClients: make(map[string]*gsrv.ServiceGRPCClient)}
	defer func() {
		if err != nil {
			err = errors.Join(err, c.Close())
		}
	}()
	for _, dependencyName := range slices.Sorted(maps.Keys(clients)) {
		cfg := clients[dependencyName]
		dependency, err := gsrv.NewDependency(dependencyName, cfg.Protection, metrics)
		if err != nil {
			return nil, fmt.Errorf("build client %s: %w", dependencyName, err)
		}
		if cfg.HTTP != nil {
			client, err := gsrv.NewServiceHTTPClient(*cfg.HTTP, cfg.Credential, dependency)
			if err != nil {
				return nil, fmt.Errorf("build HTTP client %s: %w", dependencyName, err)
			}
			c.HTTPClients[dependencyName] = client
		}
		if cfg.GRPC != nil {
			client, err := gsrv.NewServiceGRPCClient(*cfg.GRPC, cfg.Credential, dependency)
			if err != nil {
				return nil, fmt.Errorf("build gRPC client %s: %w", dependencyName, err)
			}
			c.GRPCClients[dependencyName] = client
		}
	}
	return c, nil
}

// Close 在入站排空后由bootstrap调用，允许错误路径及重复清理。
func (c *Components) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		var errs []error
		for _, name := range slices.Sorted(maps.Keys(c.HTTPClients)) {
			errs = append(errs, c.HTTPClients[name].Close())
		}
		for _, name := range slices.Sorted(maps.Keys(c.GRPCClients)) {
			errs = append(errs, c.GRPCClients[name].Close())
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}
