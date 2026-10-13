package grpc

import (
	"context"
	"example.com/service/internal/module/info/application"
	pb "example.com/service/pb/info/v1"
	"github.com/gtkit/gsrv"
)

// Handler 只执行 proto/application 映射；认证与校验由框架拦截器执行。
type Handler struct {
	pb.UnimplementedInfoServiceServer
	service *application.Service
}

// NewHandler 注入信息查询 application。
func NewHandler(service *application.Service) *Handler { return &Handler{service: service} }

// GetInfo 返回本进程的实际服务名称和框架分配的请求 ID。
func (h *Handler) GetInfo(ctx context.Context, _ *pb.GetInfoRequest) (*pb.GetInfoResponse, error) {
	result, err := h.service.Info(ctx)
	if err != nil {
		return nil, gsrv.ToGRPCStatus(err)
	}
	id := ""
	if scope := gsrv.RequestFromContext(ctx); scope != nil {
		id = scope.ID()
	}
	return &pb.GetInfoResponse{ServiceName: result.ServiceName, RequestId: id}, nil
}
