// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// apiServerName is the name containers use for the API server (a pod's KUBERNETES_SERVICE_HOST
// is the kubernetes Service's cluster IP; the name has to be in the serving certificate).
const apiServerName = "kubernetes.default.svc"

// cluster is an envtest API server that containers on the Docker network can reach, with RBAC
// enforced (envtest's default) and the serving certificate valid for apiServerName.
type cluster struct {
	env *envtest.Environment
	// dir holds the serving certificates, which envtest does not remove when it did not create them.
	dir    string
	Admin  *kubernetes.Clientset
	Port   string
	CACert []byte
}

// startCluster (call it from a spec setup node; it registers its own cleanup, before anything
// that can leave processes behind) starts the API server on all interfaces. envtest's own serving certificate names
// only localhost, so the suite generates the one the pods verify: envtest uses the certificates
// it finds in CertDir.
func startCluster() *cluster {
	dir, err := os.MkdirTemp("", "koinon-chartsmoke-apiserver-")
	if err != nil {
		panic(err)
	}
	c := &cluster{dir: dir}
	DeferCleanup(c.Stop)
	caPEM := writeAPIServerCerts(dir)

	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()

	env := &envtest.Environment{}
	c.env = env
	api := env.ControlPlane.GetAPIServer()
	api.CertDir = dir
	api.Address = "0.0.0.0"
	api.Port = port
	api.CA = caPEM // envtest sets it only when it generates the certificates itself
	cfg, err := env.Start()
	if err != nil {
		panic(fmt.Sprintf("start envtest (KUBEBUILDER_ASSETS=%q): %v", os.Getenv("KUBEBUILDER_ASSETS"), err))
	}
	cfg.Host = "https://127.0.0.1:" + port
	cfg.TLSClientConfig = rest.TLSClientConfig{CAData: caPEM, CertData: cfg.CertData, KeyData: cfg.KeyData}
	c.Admin, c.Port, c.CACert = kubernetes.NewForConfigOrDie(cfg), port, caPEM
	return c
}

func (c *cluster) Stop() {
	if c.env != nil {
		_ = c.env.Stop()
	}
	_ = os.RemoveAll(c.dir)
}

// Token is a ServiceAccount token as the kubelet would project it into a pod.
func (c *cluster) Token(ctx context.Context, sa string) string {
	tr, err := c.Admin.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, sa, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(3600))},
	}, metav1.CreateOptions{})
	if err != nil {
		panic(fmt.Sprintf("token for ServiceAccount %s: %v", sa, err))
	}
	return tr.Status.Token
}

// Secret returns a Secret, or nil when it does not exist.
func (c *cluster) Secret(ctx context.Context, name string) *corev1.Secret {
	s, err := c.Admin.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		panic(fmt.Sprintf("get Secret %s: %v", name, err))
	}
	return s
}

// writeAPIServerCerts writes what envtest would generate itself (apiserver.crt/key and the
// service-account signing pair) with apiServerName among the serving certificate's names. It
// returns the CA certificate.
func writeAPIServerCerts(dir string) []byte {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must(err)
		return k
	}
	serial := func() *big.Int {
		n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
		must(err)
		return n
	}
	pemKey := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalECPrivateKey(k)
		must(err)
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	}
	pemCert := func(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }
	now := time.Now().Add(-time.Minute)

	caKey := key()
	caTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "koinon-chartsmoke-ca"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	must(err)
	ca, err := x509.ParseCertificate(caDER)
	must(err)

	srvKey := key()
	srvDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "kube-apiserver"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost", apiServerName},
		// envtest itself dials the address the server listens on (0.0.0.0).
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback, net.IPv4zero},
	}, ca, &srvKey.PublicKey, caKey)
	must(err)

	saKey := key()
	saTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "koinon-chartsmoke-sa-signer"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	saDER, err := x509.CreateCertificate(rand.Reader, saTmpl, saTmpl, &saKey.PublicKey, saKey)
	must(err)

	for name, data := range map[string][]byte{
		"apiserver.crt": pemCert(srvDER),
		"apiserver.key": pemKey(srvKey),
		"sa-signer.crt": pemCert(saDER),
		"sa-signer.key": pemKey(saKey),
	} {
		must(os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	return pemCert(caDER)
}
