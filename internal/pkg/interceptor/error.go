package interceptor

import (
	"github.com/gtkit/gsrv/internal/pkg/fault"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToStatus 保留已映射的 gRPC status，统一取消、治理错误及未知错误的安全响应。
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	code := codes.Internal
	switch fault.CodeOf(err) {
	case fault.NotFound:
		code = codes.NotFound
	case fault.InvalidArgument:
		code = codes.InvalidArgument
	case fault.Unauthenticated:
		code = codes.Unauthenticated
	case fault.PermissionDenied:
		code = codes.PermissionDenied
	case fault.ResourceExhausted, fault.PayloadTooLarge:
		code = codes.ResourceExhausted
	case fault.Unavailable:
		code = codes.Unavailable
	case fault.DeadlineExceeded:
		code = codes.DeadlineExceeded
	case fault.Canceled:
		code = codes.Canceled
	}
	return status.Error(code, fault.Message(err))
}
