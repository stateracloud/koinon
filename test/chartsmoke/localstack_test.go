// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/testing/containerhost"
)

// localStackImage is the AWS KMS and STS the awskms mode runs against (the version
// internal/secretstore/awskms is tested with: the last one that runs without a LocalStack account).
// It checks neither signatures nor IAM policies, so this mode proves the startup inputs (the
// region, the key, the endpoints, the CA, the IRSA identity) and the wrapping; which identity may
// encrypt is covered by the adapter's own tests.
const localStackImage = containerhost.LocalStackImage

// localStackAdminKeyID is the access key the suite's administrative calls carry: obviously fake,
// never checked by LocalStack, which only reads the region from the credential scope.
const localStackAdminKeyID = "koinon_test_not-a-real-key-id"

// irsaTokenDir is where the EKS Pod Identity Webhook mounts the web identity token.
const irsaTokenDir = "/var/run/secrets/eks.amazonaws.com/serviceaccount"

// irsaRoleAnnotation is the ServiceAccount annotation IRSA reads the IAM role from.
const irsaRoleAnnotation = "eks.amazonaws.com/role-arn"

// startLocalStack runs the AWS KMS the render points at, as an operator would set it up for
// Koinon: TLS with a CA in the ConfigMap the chart mounts (the private endpoint of a proxy or a VPC
// endpoint), a symmetric key under the alias the chart names, and STS for the pods' IRSA sign-in.
func startLocalStack(ctx context.Context, cl *cluster, nwName string, k *kmsSettings) {
	GinkgoHelper()
	u, err := url.Parse(k.endpoint)
	Expect(err).NotTo(HaveOccurred())
	Expect(u.Scheme).To(Equal("https"), "the smoke test serves LocalStack with TLS")
	host, port := u.Hostname(), u.Port()
	Expect(port).NotTo(BeEmpty(), "name the port in secrets.kek.kms.awskms.endpoint")
	Expect(k.stsEndpoint).To(Equal(k.endpoint), "LocalStack serves STS at the KMS endpoint; set secrets.kek.kms.awskms.stsEndpoint to it")
	Expect(k.keyID).To(HavePrefix("alias/"), "the smoke test creates the key under the alias the chart names")
	Expect(k.caConfigMap).NotTo(BeEmpty(), "the pods need the CA of LocalStack's certificate (secrets.kek.kms.awskms.caConfigMap)")

	caPEM, certPEM, keyPEM := serverCerts(host)
	// LocalStack reads its custom certificate as one PEM file holding the key and the certificate.
	// It writes the parts next to it when it can, and falls back to a temporary file otherwise.
	pemFile := append(append([]byte{}, keyPEM...), certPEM...)
	ctr, err := testcontainers.Run(ctx, localStackImage,
		testcontainers.WithEnv(map[string]string{
			"SERVICES":               "kms,sts",
			"GATEWAY_LISTEN":         "0.0.0.0:" + port,
			"CUSTOM_SSL_CERT_PATH":   "/etc/koinon-tls/server.pem",
			"SKIP_SSL_CERT_DOWNLOAD": "1",
			"DISABLE_EVENTS":         "1",
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: bytes.NewReader(pemFile), ContainerFilePath: "/etc/koinon-tls/server.pem", FileMode: 0o644}),
		testcontainers.WithExposedPorts(port+"/tcp"),
		network.WithNetworkName([]string{host}, nwName),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready.").WithStartupTimeout(3*time.Minute)),
	)
	if ctr != nil {
		DeferCleanup(func(context.Context) { _ = testcontainers.TerminateContainer(ctr) })
	}
	Expect(err).NotTo(HaveOccurred())
	h, err := containerhost.Host(ctx, ctr)
	Expect(err).NotTo(HaveOccurred())
	mapped, err := ctr.MappedPort(ctx, port+"/tcp")
	Expect(err).NotTo(HaveOccurred())
	// The suite calls the gateway in plain HTTP, which it serves on the same port as TLS.
	ls := localStackAPI{base: fmt.Sprintf("http://%s:%s", h, mapped.Port()), region: k.region}

	key := ls.kms(ctx, "CreateKey", map[string]any{"Description": "koinon chartsmoke KEK wrapping key"})
	ls.kms(ctx, "CreateAlias", map[string]any{"AliasName": k.keyID, "TargetKeyId": key["KeyMetadata"].(map[string]any)["KeyId"]})

	_, err = cl.Admin.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: k.caConfigMap},
		Data:       map[string]string{"ca.crt": string(caPEM)},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

// localStackAPI calls LocalStack's KMS as its administrator.
type localStackAPI struct{ base, region string }

func (l localStackAPI) kms(ctx context.Context, op string, body any) map[string]any {
	GinkgoHelper()
	raw, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/", bytes.NewReader(raw))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)
	// Unsigned: LocalStack takes the region (and so the key's home) from the credential scope only.
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/kms/aws4_request, SignedHeaders=host, Signature=0",
		localStackAdminKeyID, time.Now().UTC().Format("20060102"), l.region))
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	Expect(resp.StatusCode).To(Equal(http.StatusOK), "LocalStack KMS %s: %s", op, out)
	m := map[string]any{}
	if len(out) > 0 {
		Expect(json.Unmarshal(out, &m)).To(Succeed())
	}
	return m
}

// irsaRole is the IAM role the pod's ServiceAccount names for IRSA, or "".
func irsaRole(sa *corev1.ServiceAccount) string {
	return strings.TrimSpace(sa.Annotations[irsaRoleAnnotation])
}
