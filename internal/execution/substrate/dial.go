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

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// Endpoint is how to reach Substrate: the control API (ateapi, TLS with a bearer token) and the atenet
// router (TLS on 443 with a client certificate: the router serves no plain listener and allows a client
// by the SPIFFE ID of its certificate, ADR 0119 amendment, issue #1446).
type Endpoint struct {
	// ControlAddr is ateapi's host:port, for example api.ate-system.svc:443 (the
	// ateapi certificate names that host only).
	ControlAddr string
	// RouterAddr is the router's host:port, for example atenet-router.ate-system.svc:443.
	RouterAddr string
	// RouterCAFile is the CA that signed the router's serving certificate (the servicedns signer's
	// ClusterTrustBundle). Empty uses CAFile. It is read again when it changes.
	RouterCAFile string
	// RouterServerName is the name checked against the router's certificate. Empty uses the host of
	// RouterAddr; Substrate's router certificate names atenet-router.<namespace>.svc.
	RouterServerName string
	// RouterCertFile is the client certificate of the router connection: one PEM file with the private
	// key and the chain (credential-bundle.pem of a podCertificate volume), read again when it changes.
	// Empty sends none, which the router refuses: only a binary that makes no router call leaves it empty.
	RouterCertFile string
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
	router, refusal, err := dialRouter(ep, opts...)
	if err != nil {
		_ = control.Close()
		return nil, err
	}
	d := New(control, router, cfg)
	d.routerRefusal = refusal
	d.closers = []io.Closer{control, router}
	return d, nil
}

// dialRouter connects to the atenet router. The second result returns the identity refusal that a router
// call error carries, or nil.
func dialRouter(ep Endpoint, opts ...grpc.DialOption) (*grpc.ClientConn, func(error) error, error) {
	creds, err := routerCredentials(ep)
	if err != nil {
		return nil, nil, err
	}
	ropts := append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, opts...)
	router, err := grpc.NewClient(ep.RouterAddr, ropts...)
	if err != nil {
		return nil, nil, fmt.Errorf("substrate: connect to %s: %w", ep.RouterAddr, err)
	}
	return router, creds.(*routerCreds).refused.matches, nil
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
