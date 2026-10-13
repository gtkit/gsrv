package bootstrap

import (
	"buf.build/go/protovalidate"
	infogrpc "example.com/service/internal/module/info/transport/grpc"
	"example.com/service/internal/platform/config"
	infort "example.com/service/internal/runtime/module/info"
	pb "example.com/service/pb/info/v1"
	"github.com/gtkit/gsrv"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// buildGRPCServers 沿用 bootstrap→runtime/module→application 的装配方向。
func buildGRPCServers(cfg *config.Config, state *gsrv.HealthState, verifier *gsrv.Verifier, quota gsrv.RateLimiter) (public, backend *gsrv.GRPCServer, err error) {
	validator, err := protovalidate.New()
	if err != nil {
		return nil, nil, err
	}
	validate := func(m proto.Message) error { return validator.Validate(m) }
	handler := infogrpc.NewHandler(infort.Build(cfg.Name))
	register := func(registrar grpc.ServiceRegistrar) { pb.RegisterInfoServiceServer(registrar, handler) }
	if cfg.GRPC != nil {
		public, err = gsrv.NewGRPCServer(cfg.Name, *cfg.GRPC, gsrv.WithGRPCAuth(verifier), gsrv.WithGRPCLimiter(quota), gsrv.WithGRPCHealth(state), gsrv.WithGRPCValidation(validate))
		if err != nil {
			return nil, nil, err
		}
		public.Register(register)
	}
	if cfg.GatewaySocket != "" {
		backend, err = gsrv.NewGatewayBackend(cfg.Name, gsrv.GRPCConfig{Network: "unix", Addr: cfg.GatewaySocket}, gsrv.WithGRPCHealth(state), gsrv.WithGRPCValidation(validate))
		if err != nil {
			return nil, nil, err
		}
		backend.Register(register)
	}
	return public, backend, nil
}
