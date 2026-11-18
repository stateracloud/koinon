// SPDX-License-Identifier: Apache-2.0

package faketls_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stateracloud/koinon/internal/faketls"
)

func TestSelfSignedVerifiesForItsHosts(t *testing.T) {
	cert, caPEM, err := faketls.SelfSigned("test", []string{"fakemcp.example.svc", "127.0.0.1", " "})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("no PEM block")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Error("the server certificate is a CA: strict clients refuse it")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the PEM is not a certificate")
	}
	for _, host := range []string{"fakemcp.example.svc", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example"}); err == nil {
		t.Error("a host that is not in the certificate verified")
	}
}
