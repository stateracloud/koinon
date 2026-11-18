// SPDX-License-Identifier: Apache-2.0

// Package faketls makes the throw-away certificates of the fake servers (koinon-fakemcp) for tests and
// e2e. Nothing here is for production. A test run makes one set with Make and hands it to the fake as a
// run-time Secret, so a pod restart keeps the same CA and the server record that trusts it stays valid.
// No key is committed, and the fake never makes its own.
package faketls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// Set is one CA and one server certificate it signed, as PEM.
type Set struct {
	// CA is the PEM of the CA certificate: what a server record's CA bundle holds. Public.
	CA []byte
	// Cert and Key are the server certificate and its private key. The key goes only into the
	// run-time Secret of the fake, never into a file in the tree.
	Cert, Key []byte
}

// Make makes a CA and a server certificate for hosts (names and IPs) that the CA signs. The server
// certificate is not a CA: strict clients (rustls, which agentgateway uses) refuse a CA certificate as
// a server certificate. commonName names the CA.
func Make(commonName string, hosts []string) (Set, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Set{}, err
	}
	caTpl := &x509.Certificate{
		SerialNumber:          serial(0),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return Set{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Set{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Set{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(1),
		Subject:               pkix.Name{CommonName: commonName + " server"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	if len(tpl.DNSNames)+len(tpl.IPAddresses) == 0 {
		return Set{}, errors.New("faketls: no host to make a certificate for")
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return Set{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Set{}, err
	}
	return Set{
		CA:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Load reads a server certificate and key (PEM files) for a TLS server.
func Load(certFile, keyFile string) (tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("faketls: %w", err)
	}
	return c, nil
}

func serial(n int64) *big.Int { return big.NewInt(time.Now().UnixNano() + n) }
