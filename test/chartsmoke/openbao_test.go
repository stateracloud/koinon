// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/moby/moby/api/types/container"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/testing/containerhost"
)

// openBaoImage is the OpenBao the KMS mode runs against (the version internal/secretstore/openbao
// is tested with).
const openBaoImage = "docker.io/openbao/openbao:2.7.2"

// openBaoRootToken is the dev server's root token: obviously fake, and only the test container's.
const openBaoRootToken = "koinon_test_not-a-real-token"

// kmsSettings is what the rendered pods expect of the KMS, read from their env: the backend
// (KOINON_KEK_BACKEND) and its settings (KOINON_OPENBAO_* or KOINON_AWSKMS_*).
type kmsSettings struct {
	backend string
	// OpenBao: the server the chart points at, its transit key and Kubernetes auth mount, and the
	// role each ServiceAccount signs in as.
	address, transitMount, key, authMount string
	// roles maps an OpenBao role to the ServiceAccount it binds; admin roles may encrypt.
	roles map[string]string
	admin map[string]bool
	// AWS KMS: the region, the key and the endpoints the pods call.
	region, keyID, endpoint, stsEndpoint string
	// caConfigMap is the ConfigMap the pods mount the KMS's CA from ("" when none).
	caConfigMap string
}

// kmsOf returns the KMS settings of the render, or nil when the KEKs stay local.
func kmsOf(r *rendered) *kmsSettings {
	var k *kmsSettings
	for _, ps := range r.podSpecs() {
		for _, c := range append(slices.Clone(ps.InitContainers), ps.Containers...) {
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			backend := env["KOINON_KEK_BACKEND"]
			if backend == "" || backend == "local" {
				continue
			}
			if k == nil {
				k = &kmsSettings{backend: backend, roles: map[string]string{}, admin: map[string]bool{}}
			}
			Expect(backend).To(Equal(k.backend), "%s uses another KMS than the other pods", c.Name)
			sa := ps.ServiceAccountName
			if sa == "" {
				sa = "default"
			}
			caVolume := ""
			switch backend {
			case "openbao":
				caVolume = "openbao-ca"
				k.address, k.transitMount, k.key, k.authMount = env["KOINON_OPENBAO_ADDRESS"], env["KOINON_OPENBAO_TRANSIT_MOUNT"], env["KOINON_OPENBAO_KEY"], env["KOINON_OPENBAO_AUTH_MOUNT"]
				role := env["KOINON_OPENBAO_ROLE"]
				Expect(role).NotTo(BeEmpty(), "%s signs in to OpenBao with a role", c.Name)
				if prev, ok := k.roles[role]; ok {
					Expect(prev).To(Equal(sa), "OpenBao role %s is used by two ServiceAccounts", role)
				}
				k.roles[role] = sa
				k.admin[role] = k.admin[role] || slices.Contains(c.Args, "key-admin")
			case "awskms":
				caVolume = "awskms-ca"
				k.region, k.keyID, k.endpoint, k.stsEndpoint = env["KOINON_AWSKMS_REGION"], env["KOINON_AWSKMS_KEY_ID"], env["KOINON_AWSKMS_ENDPOINT"], env["KOINON_AWSKMS_STS_ENDPOINT"]
			default:
				Fail(fmt.Sprintf("%s: the smoke test runs no KMS for backend %q", c.Name, backend))
			}
			for _, v := range ps.Volumes {
				if v.Name == caVolume && v.ConfigMap != nil {
					k.caConfigMap = v.ConfigMap.Name
				}
			}
		}
	}
	return k
}

// start runs the KMS the render points at, set up as the operator would before the install. Call
// it once the namespace exists.
func (k *kmsSettings) start(ctx context.Context, cl *cluster, nwName string) {
	GinkgoHelper()
	switch k.backend {
	case "openbao":
		startOpenBao(ctx, cl, nwName, k)
	case "awskms":
		startLocalStack(ctx, cl, nwName, k)
	}
}

// keyName is how a KEK wrapped by this KMS names the key (secretstore.Wrapper.KeyID).
func (k *kmsSettings) keyName() string {
	if k.backend == "openbao" {
		return k.transitMount + "/" + k.key
	}
	return k.keyID
}

// startOpenBao runs the OpenBao the render points at, as an operator would set it up for Koinon:
// TLS with a CA in the ConfigMap the chart mounts, the transit key, Kubernetes auth against the
// suite's API server, a decrypt-only policy for koinon-api and the reconciler and an encrypt policy
// for the key-admin Job (update, never create: ADR 0085 decision 12). Call it once the namespace
// exists.
func startOpenBao(ctx context.Context, cl *cluster, nwName string, k *kmsSettings) {
	GinkgoHelper()
	u, err := url.Parse(k.address)
	Expect(err).NotTo(HaveOccurred())
	Expect(u.Scheme).To(Equal("https"), "the smoke test serves OpenBao with TLS")
	host, port := u.Hostname(), u.Port()
	Expect(port).NotTo(BeEmpty(), "name the port in secrets.kek.kms.openbao.address")

	caPEM, certPEM, keyPEM := serverCerts(host)
	listener := fmt.Sprintf(`listener "tcp" {
  address       = "0.0.0.0:%s"
  tls_cert_file = "/openbao/tls/tls.crt"
  tls_key_file  = "/openbao/tls/tls.key"
}
`, port)
	file := func(name string, data []byte) testcontainers.ContainerFile {
		return testcontainers.ContainerFile{Reader: bytes.NewReader(data), ContainerFilePath: "/openbao/tls/" + name, FileMode: 0o644}
	}
	ctr, err := testcontainers.Run(ctx, openBaoImage,
		// The dev listener (plain HTTP) is how the suite sets OpenBao up; the pods use the TLS one.
		testcontainers.WithCmd("server", "-dev", "-dev-listen-address=0.0.0.0:8200", "-config=/openbao/tls/listener.hcl"),
		testcontainers.WithEnv(map[string]string{"BAO_DEV_ROOT_TOKEN_ID": openBaoRootToken}),
		testcontainers.WithFiles(file("tls.crt", certPEM), file("tls.key", keyPEM), file("listener.hcl", []byte(listener))),
		testcontainers.WithExposedPorts("8200/tcp"),
		network.WithNetworkName([]string{host}, nwName),
		// OpenBao reviews the pods' tokens with the API server, as it would in a cluster.
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.ExtraHosts = append(hc.ExtraHosts, apiServerName+":host-gateway")
		}),
		// The dev listener is IPv4 only: wait.ForHTTP would dial "localhost", which is ::1 first on some
		// machines (OrbStack). Wait for the log line, then poll the health on the IPv4 host below. The
		// deadline is explicit: WithWaitStrategy's default of 60 s would override the startup timeout.
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForLog("Development mode should NOT be used").WithStartupTimeout(2*time.Minute)),
	)
	if ctr != nil {
		DeferCleanup(func(context.Context) { _ = testcontainers.TerminateContainer(ctr) })
	}
	Expect(err).NotTo(HaveOccurred())
	h, err := containerhost.Host(ctx, ctr)
	Expect(err).NotTo(HaveOccurred())
	mapped, err := ctr.MappedPort(ctx, "8200/tcp")
	Expect(err).NotTo(HaveOccurred())
	bao := openBaoAPI{base: fmt.Sprintf("http://%s:%s", h, mapped.Port())}
	Eventually(func(g Gomega) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, bao.base+"/v1/sys/health", nil)
		g.Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		g.Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}).WithTimeout(time.Minute).Should(Succeed())

	bao.post(ctx, "/v1/sys/mounts/"+k.transitMount, map[string]any{"type": "transit"})
	bao.post(ctx, "/v1/"+k.transitMount+"/keys/"+k.key, map[string]any{})
	bao.post(ctx, "/v1/sys/policies/acl/koinon-decrypt", map[string]any{
		"policy": fmt.Sprintf(`path "%s/decrypt/%s" { capabilities = ["update"] }`, k.transitMount, k.key),
	})
	bao.post(ctx, "/v1/sys/policies/acl/koinon-key-admin", map[string]any{
		"policy": fmt.Sprintf("path \"%[1]s/encrypt/%[2]s\" { capabilities = [\"update\"] }\npath \"%[1]s/decrypt/%[2]s\" { capabilities = [\"update\"] }", k.transitMount, k.key),
	})

	// Kubernetes auth: OpenBao calls the TokenReview API with a reviewer's token, the setup the
	// operator guide describes for an OpenBao outside the cluster.
	reviewer := openBaoReviewer(ctx, cl)
	bao.post(ctx, "/v1/sys/auth/"+k.authMount, map[string]any{"type": "kubernetes"})
	bao.post(ctx, "/v1/auth/"+k.authMount+"/config", map[string]any{
		"kubernetes_host":        "https://" + apiServerName + ":" + cl.Port,
		"kubernetes_ca_cert":     string(cl.CACert),
		"token_reviewer_jwt":     reviewer,
		"disable_local_ca_jwt":   true,
		"disable_iss_validation": true,
	})
	for role, sa := range k.roles {
		policy := "koinon-decrypt"
		if k.admin[role] {
			policy = "koinon-key-admin"
		}
		bao.post(ctx, "/v1/auth/"+k.authMount+"/role/"+role, map[string]any{
			"bound_service_account_names":      []string{sa},
			"bound_service_account_namespaces": []string{namespace},
			"token_policies":                   []string{policy},
			"token_ttl":                        "1h",
		})
	}

	if k.caConfigMap != "" {
		_, err := cl.Admin.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: k.caConfigMap},
			Data:       map[string]string{"ca.crt": string(caPEM)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
}

// openBaoReviewer creates a ServiceAccount allowed to review tokens and returns a token of it.
func openBaoReviewer(ctx context.Context, cl *cluster) string {
	const name = "openbao-token-reviewer"
	_, err := cl.Admin.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	_, err = cl.Admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}}},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	_, err = cl.Admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	return cl.Token(ctx, name)
}

// openBaoAPI calls the dev server as root.
type openBaoAPI struct{ base string }

func (b openBaoAPI) post(ctx context.Context, path string, body any) {
	GinkgoHelper()
	raw, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+path, bytes.NewReader(raw))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("X-Vault-Token", openBaoRootToken)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	Expect(resp.StatusCode).To(BeNumerically("<", 300), "OpenBao %s: %s", path, out)
}

// serverCerts is a throwaway CA and a serving certificate for host.
func serverCerts(host string) (caPEM, certPEM, keyPEM []byte) {
	now := time.Now().Add(-time.Minute)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "koinon-chartsmoke-kms-ca"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	must(err)
	ca, err := x509.ParseCertificate(caDER)
	must(err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour), DNSNames: []string{host},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	must(err)
	kder, err := x509.MarshalECPrivateKey(key)
	must(err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}
