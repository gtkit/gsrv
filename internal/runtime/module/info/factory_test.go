package info

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gtkit/gsrv"
	pb "github.com/gtkit/gsrv/pb/info/v1"
)

func TestOptionalClientAssemblyAndClose(t *testing.T) {
	credential := gsrv.Credential{AppKey: "self", KeyID: "primary", Secret: strings.Repeat("s", 32), Token: strings.Repeat("t", 32)}
	clients := map[string]gsrv.DownstreamConfig{"peer": {Credential: credential, HTTP: &gsrv.ServiceHTTPConfig{Service: "peer-service", BaseURL: "http://127.0.0.1:8081"}, GRPC: &gsrv.ServiceGRPCConfig{Target: "127.0.0.1:9091", Plaintext: true}}}
	module, err := Build("info", clients, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(module.HTTPClients) != 1 || len(module.GRPCClients) != 1 {
		t.Fatal("clients not assembled")
	}
	result, err := module.Service.Info(t.Context())
	if err != nil || result.ServiceName != "info" {
		t.Fatal("InfoService changed")
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := module.HTTPClients["peer"].Do(t.Context(), "GET", "/", nil, nil); gsrv.ErrorCodeOf(err) != gsrv.CodeUnavailable {
		t.Fatalf("HTTP not closed=%v", err)
	}
	if _, err := pb.NewInfoServiceClient(module.GRPCClients["peer"]).GetInfo(context.WithoutCancel(t.Context()), &pb.GetInfoRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("gRPC not closed=%v", err)
	}
	clients["peer"] = gsrv.DownstreamConfig{Credential: credential, HTTP: &gsrv.ServiceHTTPConfig{Service: "peer", BaseURL: "http://127.0.0.1:8081"}, GRPC: &gsrv.ServiceGRPCConfig{Target: "invalid"}}
	if _, err := Build("info", clients, nil); err == nil {
		t.Fatal("invalid mixed assembly accepted")
	}
}
