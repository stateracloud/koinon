// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// Endpoint is how to reach Substrate: the control API (ateapi, TLS with a pod certificate) and the
// atenet router (plain HTTP/2 inside the cluster; the install allows the router's ingress by SPIFFE
// ID, ADR 0119 proof findings).
type Endpoint struct {
	// ControlAddr is ateapi's host:port, for example api.ate-system.svc.cluster.local:443.
	ControlAddr string
	// RouterAddr is the router's host:port, for example atenet-router.ate-system.svc.cluster.local:80.
	RouterAddr string
	// CAFile is the CA that signed ateapi's certificate. Empty uses the system roots.
	CAFile string
	// ServerName overrides the name checked against ateapi's certificate.
	ServerName string
	// BearerTokenFile holds a projected ServiceAccount token with the ateapi audience. The file is
	// read again on every call, so a rotated token is picked up. Empty sends no token.
	BearerTokenFile string
}

// DialControl connects to the control API (ateapi) only, with the bearer token of ep. Close the returned
// connection when done. The provisioner uses it: it has no use for the router.
func DialControl(ep Endpoint, opts ...grpc.DialOption) (ateapiv1.ControlClient, io.Closer, error) {
	conn, err := dialControlConn(ep, opts...)
	if err != nil {
		return nil, nil, err
	}
	return ateapiv1.NewControlClient(conn), conn, nil
}

func dialControlConn(ep Endpoint, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if ep.ControlAddr == "" {
		return nil, errors.New("substrate: ControlAddr is required")
	}
	tlsCfg := &tls.Config{ServerName: ep.ServerName, MinVersion: tls.VersionTLS12}
	if ep.CAFile != "" {
		ca, err := os.ReadFile(ep.CAFile)
		if err != nil {
			return nil, fmt.Errorf("substrate: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("substrate: no certificates in %s", ep.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	copts := append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}, opts...)
	if ep.BearerTokenFile != "" {
		copts = append(copts, grpc.WithPerRPCCredentials(fileToken{path: ep.BearerTokenFile}))
	}
	control, err := grpc.NewClient(ep.ControlAddr, copts...)
	if err != nil {
		return nil, fmt.Errorf("substrate: connect to %s: %w", ep.ControlAddr, err)
	}
	return control, nil
}

// Dial connects to Substrate. Close releases the connections.
func Dial(ep Endpoint, cfg Config, opts ...grpc.DialOption) (*Driver, error) {
	if ep.ControlAddr == "" || ep.RouterAddr == "" {
		return nil, errors.New("substrate: ControlAddr and RouterAddr are required")
	}
	control, err := dialControlConn(ep, opts...)
	if err != nil {
		return nil, err
	}
	ropts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	router, err := grpc.NewClient(ep.RouterAddr, ropts...)
	if err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("substrate: connect to %s: %w", ep.RouterAddr, err)
	}
	d := New(control, router, cfg)
	d.closers = []io.Closer{control, router}
	return d, nil
}

// fileToken sends the contents of a file as a bearer token.
type fileToken struct{ path string }

func (f fileToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("substrate: read bearer token: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

func (fileToken) RequireTransportSecurity() bool { return true }
