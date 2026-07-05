package controlplane

import (
	"context"
	"net"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Register registers both contract-plane services on a gRPC server.
func (s *Server) Register(gs *grpc.Server) {
	arkwenv1.RegisterReadPlaneServer(gs, s)
	arkwenv1.RegisterCommandPlaneServer(gs, s)
}

// Serve runs the contract plane on lis until ctx is canceled. (Transport security
// — mTLS/OIDC — is layered above via grpc.ServerOption in production; the AuthN
// boundary is the authz.Authenticator.)
func Serve(ctx context.Context, lis net.Listener, s *Server, opts ...grpc.ServerOption) error {
	gs := grpc.NewServer(opts...)
	s.Register(gs)
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	return gs.Serve(lis)
}

// Dial opens a client connection to a control-plane target (insecure transport;
// production layers TLS). Consumer-agnostic: the client drives Arkwen one-way.
func Dial(target string) (*grpc.ClientConn, error) {
	return grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// WithToken attaches the caller's credential to the outgoing context. The token
// is verified-and-discarded server-side (structural exclusion, Invariant 5).
func WithToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", token)
}
