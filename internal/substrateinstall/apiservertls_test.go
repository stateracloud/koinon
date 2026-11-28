// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	certsv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// testCA makes a CA and a serving certificate for dns, and returns the CA in PEM and the certificate.
func testCA(t *testing.T, dns string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dns}, DNSNames: []string{dns},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

// handshake dials a TLS server that presents cert and reports whether cfg accepts it.
func handshake(t *testing.T, cert tls.Certificate, cfg *tls.Config) error {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		if c, err := l.Accept(); err == nil {
			_ = c.(*tls.Conn).HandshakeContext(context.Background())
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", l.Addr().(*net.TCPAddr).String())
	if err == nil {
		_ = c.Close()
	}
	return err
}

func bundle(name, signer, pemCA string, labels map[string]string) *certsv1.ClusterTrustBundle {
	return &certsv1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       certsv1.ClusterTrustBundleSpec{SignerName: signer, TrustBundle: pemCA},
	}
}

var liveLabel = map[string]string{"podcert.ate.dev/canarying": "live"}

func TestAPIServerTLSVerifiesTheServerCertificate(t *testing.T) {
	ca, cert := testCA(t, "api.ate-system.svc")
	otherCA, _ := testCA(t, "api.ate-system.svc")
	signer := "servicedns.podcert.ate.dev/identity"

	t.Run("the right CA passes", func(t *testing.T) {
		g := NewWithT(t)
		i, _, _ := setup("37", true, []runtime.Object{bundle("primary-bundle", signer, string(ca), liveLabel)})
		cfg, err := i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cfg.ServerName).To(Equal("api.ate-system.svc"))
		g.Expect(cfg.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
		g.Expect(handshake(t, cert, cfg)).To(Succeed())
	})
	t.Run("a wrong CA fails", func(t *testing.T) {
		g := NewWithT(t)
		i, _, _ := setup("37", true, []runtime.Object{bundle("b", signer, string(otherCA), liveLabel)})
		cfg, err := i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(handshake(t, cert, cfg)).To(HaveOccurred())
	})
	t.Run("a wrong server name fails", func(t *testing.T) {
		g := NewWithT(t)
		i, _, _ := setup("37", true, []runtime.Object{bundle("b", signer, string(ca), liveLabel)})
		cfg, err := i.APIServerTLS(context.Background(), "other-ns")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(handshake(t, cert, cfg)).To(HaveOccurred())
	})
	t.Run("no CA fails closed", func(t *testing.T) {
		g := NewWithT(t)
		i, _, _ := setup("37", true, nil)
		_, err := i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).To(MatchError(ContainSubstring("no ClusterTrustBundle")))
	})
	t.Run("a bundle of another signer or without the live label is not a CA", func(t *testing.T) {
		g := NewWithT(t)
		i, _, _ := setup("37", true, []runtime.Object{
			bundle("a", "podidentity.podcert.ate.dev/identity", string(ca), liveLabel),
			bundle("b", signer, string(ca), nil),
		})
		_, err := i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).To(MatchError(ContainSubstring("no ClusterTrustBundle of signer servicedns.podcert.ate.dev/identity")))
	})
	t.Run("a rotated CA passes when only the second live bundle holds it", func(t *testing.T) {
		g := NewWithT(t)
		i, _, _ := setup("37", true, []runtime.Object{
			bundle("a", signer, string(otherCA), liveLabel),
			bundle("b", signer, string(ca), liveLabel),
		})
		cfg, err := i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(handshake(t, cert, cfg)).To(Succeed())
	})
	t.Run("--api-ca-file replaces the bundle", func(t *testing.T) {
		g := NewWithT(t)
		f := filepath.Join(t.TempDir(), "ca.pem")
		g.Expect(os.WriteFile(f, ca, 0o600)).To(Succeed())
		i, _, _ := setup("37", true, nil)
		i.APICAFile = f
		cfg, err := i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(handshake(t, cert, cfg)).To(Succeed())
		g.Expect(os.WriteFile(f, []byte("junk"), 0o600)).To(Succeed())
		_, err = i.APIServerTLS(context.Background(), "ate-system")
		g.Expect(err).To(MatchError(ContainSubstring("no PEM certificate")))
	})
}
