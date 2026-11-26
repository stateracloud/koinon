// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc/credentials"
)

// The atenet router serves TLS only and wants a client certificate whose SPIFFE ID is on its allow-list
// (Substrate v1.0.1 cmd/atenet/internal/router/README.md, "Ingress client authentication and
// authorization"). The pod gets that certificate from a podCertificate projected volume and the CA of the
// router's serving certificate from a clusterTrustBundle projected volume (the "Client in the cluster, pod
// identity CA" recipe of the README). The kubelet rotates both files in place, so this file reads them
// again when they change: a pod that keeps its first certificate would fail at the next rotation.

// watchedFile is a file that is read again when it changes on disk. The kubelet replaces a projected file
// by switching a symbolic link, so a changed file has another inode, time or size.
type watchedFile struct {
	path string

	mu   sync.Mutex
	info os.FileInfo
	data []byte
}

// read returns the current content of the file.
func (w *watchedFile) read() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fi, err := os.Stat(w.path)
	if err != nil {
		return nil, err
	}
	if w.info != nil && os.SameFile(w.info, fi) && w.info.ModTime().Equal(fi.ModTime()) && w.info.Size() == fi.Size() {
		return w.data, nil
	}
	b, err := os.ReadFile(w.path)
	if err != nil {
		return nil, err
	}
	w.info, w.data = fi, b
	return b, nil
}

// clientCert is the router client certificate: one PEM file with the private key and the chain, the
// credential-bundle.pem of a podCertificate projection.
type clientCert struct {
	file *watchedFile

	mu   sync.Mutex
	from []byte
	cert *tls.Certificate
}

func (c *clientCert) get() (*tls.Certificate, error) {
	b, err := c.file.read()
	if err != nil {
		return nil, fmt.Errorf("read the router client certificate: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && string(c.from) == string(b) {
		return c.cert, nil
	}
	// The bundle holds the key and the chain; X509KeyPair takes each from its own blocks.
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("router client certificate %s: %w", c.file.path, err)
	}
	c.from, c.cert = b, &cert
	return c.cert, nil
}

// roots is the CA of the router's serving certificate, read again when its file changes.
type roots struct {
	file *watchedFile // nil: the system roots

	mu   sync.Mutex
	from []byte
	pool *x509.CertPool
}

func (r *roots) get() (*x509.CertPool, error) {
	if r.file == nil {
		return nil, nil // x509 uses the system roots
	}
	b, err := r.file.read()
	if err != nil {
		return nil, fmt.Errorf("read the router CA: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool != nil && string(r.from) == string(b) {
		return r.pool, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("substrate: no certificates in the router CA %s", r.file.path)
	}
	r.from, r.pool = b, pool
	return pool, nil
}

// routerCredentials builds the TLS credentials of the router connection. The server certificate is
// checked against the CA file and the server name; the client certificate, when there is one, is read
// again at every handshake.
func routerCredentials(ep Endpoint) (credentials.TransportCredentials, error) {
	name := ep.RouterServerName
	if name == "" {
		host, _, err := net.SplitHostPort(ep.RouterAddr)
		if err != nil {
			return nil, fmt.Errorf("substrate: RouterAddr %q: %w", ep.RouterAddr, err)
		}
		name = host
	}
	caPath := ep.RouterCAFile
	if caPath == "" {
		caPath = ep.CAFile
	}
	ca := &roots{}
	if caPath != "" {
		ca.file = &watchedFile{path: caPath}
		if _, err := ca.get(); err != nil { // fail at dial time, not at the first command
			return nil, err
		}
	}
	rc := &routerCreds{name: name, ca: ca}
	if ep.RouterCertFile != "" {
		cc := &clientCert{file: &watchedFile{path: ep.RouterCertFile}}
		if _, err := cc.get(); err != nil {
			return nil, err
		}
		rc.clientCert = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cc.get() }
	}
	return rc, nil
}

// routerCreds are gRPC transport credentials that build a new tls.Config for every handshake. The standard
// verification (chain, name, ServerAuth key usage) then runs against the CA pool of that moment, so a
// rotation of the router's CA needs no restart and no verification is skipped.
type routerCreds struct {
	name       string
	ca         *roots
	clientCert func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
}

func (c *routerCreds) config() (*tls.Config, error) {
	pool, err := c.ca.get()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		ServerName:           c.name,
		RootCAs:              pool, // nil: the system roots
		GetClientCertificate: c.clientCert,
	}, nil
}

func (c *routerCreds) ClientHandshake(ctx context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, err := c.config()
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	// Check the router against the pinned name, not the channel authority that grpc passes in.
	return credentials.NewTLS(cfg).ClientHandshake(ctx, c.name, conn)
}

func (c *routerCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("substrate: the router credentials are client side only")
}

func (c *routerCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}

func (c *routerCreds) Clone() credentials.TransportCredentials {
	cp := *c
	return &cp
}

// OverrideServerName sets the name the router certificate is checked against.
func (c *routerCreds) OverrideServerName(name string) error {
	c.name = name
	return nil
}
