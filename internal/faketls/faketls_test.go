// SPDX-License-Identifier: Apache-2.0

package faketls_test

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/stateracloud/koinon/internal/faketls"
)

func TestSetVerifiesAgainstItsCAOnlyAndNamesItsHosts(t *testing.T) {
	s, err := faketls.Make("koinon-faketls test CA", []string{"fakemcp.test.svc", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, s.Cert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, s.Key, 0o600); err != nil {
		t.Fatal(err)
	}
	pair, err := faketls.Load(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Error("the server certificate must not be a CA (rustls refuses it)")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.CA) {
		t.Fatal("the CA PEM holds no certificate")
	}
	for _, host := range []string{"fakemcp.test.svc", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example.com"}); err == nil {
		t.Error("the certificate must not verify for a host it was not made for")
	}
	other, err := faketls.Make("another CA", []string{"fakemcp.test.svc"})
	if err != nil {
		t.Fatal(err)
	}
	otherPool := x509.NewCertPool()
	otherPool.AppendCertsFromPEM(other.CA)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: otherPool, DNSName: "fakemcp.test.svc"}); err == nil {
		t.Error("another CA must not verify the certificate")
	}
}
