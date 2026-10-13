// Package info 是示例模块显式装配入口，后续仓储和 provider 在此注入。
package info

import "github.com/gtkit/gsrv/internal/module/info/application"

// Build 构造示例 application；不启动任务或连接基础设施。
func Build(name string) *application.Service { return application.New(name) }
