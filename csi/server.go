package csi

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// Server hosts the CSI gRPC services on a single Unix-domain socket endpoint.
type Server struct {
	endpoint string
	srv      *grpc.Server

	identity   csi.IdentityServer
	controller csi.ControllerServer
	node       csi.NodeServer
}

// NewServer wires the gRPC plumbing. Pass a nil controller or node to skip
// registering that service (controller-mode and node-mode each register only
// what they implement, but identity is required everywhere).
func NewServer(endpoint string, identity csi.IdentityServer, controller csi.ControllerServer, node csi.NodeServer) *Server {
	srv := grpc.NewServer(grpc.UnaryInterceptor(logInterceptor))
	if identity != nil {
		csi.RegisterIdentityServer(srv, identity)
	}
	if controller != nil {
		csi.RegisterControllerServer(srv, controller)
	}
	if node != nil {
		csi.RegisterNodeServer(srv, node)
	}
	return &Server{endpoint: endpoint, srv: srv, identity: identity, controller: controller, node: node}
}

func (s *Server) Serve(ctx context.Context) error {
	scheme, addr, err := parseEndpoint(s.endpoint)
	if err != nil {
		return err
	}
	if scheme == "unix" {
		// The parent dir may not exist yet (e.g. a Windows HostProcess node
		// plugin pointed at C:\var\lib\kubelet\plugins\<driver>\csi.sock).
		if dir := filepath.Dir(addr); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = os.Remove(addr)
	}
	lis, err := net.Listen(scheme, addr)
	if err != nil {
		return err
	}
	logrus.Infof("CSI server listening on %s://%s", scheme, addr)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		logrus.Infof("CSI server: stopping")
		s.srv.GracefulStop()
	}()
	if err := s.srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	wg.Wait()
	return nil
}

func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(ep, "unix://") {
		return "unix", strings.TrimPrefix(ep, "unix://"), nil
	}
	if strings.HasPrefix(ep, "tcp://") {
		return "tcp", strings.TrimPrefix(ep, "tcp://"), nil
	}
	return "", "", errors.New("unsupported endpoint scheme; expected unix:// or tcp://")
}

func logInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	logrus.Debugf("CSI %s req=%+v", info.FullMethod, req)
	resp, err := handler(ctx, req)
	if err != nil {
		logrus.Errorf("CSI %s: %v", info.FullMethod, err)
	} else {
		logrus.Debugf("CSI %s OK", info.FullMethod)
	}
	return resp, err
}
