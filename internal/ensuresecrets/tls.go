// SPDX-License-Identifier: Apache-2.0

package ensuresecrets

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
	"time"
)

// KindTLS is Spec.Kind of a Secret of type kubernetes.io/tls with a server certificate that a new
// CA signed, for the in-cluster image cache of the Substrate driver (issue #1170). The Secret holds
// tls.crt, tls.key and ca.crt. The CA key is not kept: the one certificate is the only one the CA
// signs, so nothing can sign another in its name. To rotate, delete the Secret and run the hook Job
// again, then give the new ca.crt to the node agent. An operator who wants their own CA sets the
// chart's existing Secret and this kind is not used.
const KindTLS = "tls"

// The data keys of a KindTLS Secret.
const (
	TLSCertKey = "tls.crt"
	TLSKeyKey  = "tls.key"
	TLSCAKey   = "ca.crt"
)

// tlsValidity is how long the CA and the certificate last. The cluster has no renewal path for the
// node agent's CA bundle yet, so a short life would break image pulls with no warning; the key is
// private to the cache pod.
const tlsValidity = 10 * 365 * 24 * time.Hour

// newTLSData returns the data of a KindTLS Secret for the DNS names: a leaf certificate for the names, the
// CA that signed it, and the leaf key, all in PEM.
func newTLSData(dnsNames []string, now time.Time) (map[string][]byte, error) {
	if len(dnsNames) == 0 {
		return nil, errors.New("a tls Secret needs at least one DNS name")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "koinon-image-cache-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(tlsValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create the CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(tlsValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create the certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		TLSCertKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		TLSKeyKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		TLSCAKey:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		panic(err) // crypto/rand does not fail on a working system
	}
	return n.Add(n, big.NewInt(1))
}

// checkTLSData says whether data is a usable KindTLS Secret: the key pair loads, the CA parses, and the
// certificate is signed by that CA and names every DNS name. An operator may have set their own, which is kept
// as it is when it passes.
func checkTLSData(data map[string][]byte, dnsNames []string, now time.Time) error {
	pair, err := tls.X509KeyPair(data[TLSCertKey], data[TLSKeyKey])
	if err != nil {
		return fmt.Errorf("the %q and %q keys are not a certificate and its key: %w", TLSCertKey, TLSKeyKey, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data[TLSCAKey]) {
		return fmt.Errorf("the %q key holds no certificate", TLSCAKey)
	}
	for _, n := range dnsNames {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: n, CurrentTime: now}); err != nil {
			return fmt.Errorf("the certificate does not verify for %s against %q: %w", n, TLSCAKey, err)
		}
	}
	return nil
}
