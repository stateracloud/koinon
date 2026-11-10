// SPDX-License-Identifier: Apache-2.0

package openbao_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/testing/containerhost"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func TestOpenBao(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OpenBao Suite")
}

// image is the OpenBao the adapter is tested against.
const image = "docker.io/openbao/openbao:2.7.2"

// rootToken is the dev server's root token: obviously fake, and only ever the test container's.
const rootToken = "koinon_test_not-a-real-token"

const (
	testKey      = "koinon-test-kek"
	decryptToken = "koinon_test_not-a-real-decrypt-token"
	adminToken   = "koinon_test_not-a-real-admin-token"
)

// bao is the OpenBao dev server of this process.
var bao string

var _ = BeforeSuite(func(ctx SpecContext) {
	testenv.Require(testenv.Docker)
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			Cmd:          []string{"server", "-dev", "-dev-listen-address=0.0.0.0:8200"},
			Env:          map[string]string{"BAO_DEV_ROOT_TOKEN_ID": rootToken},
			ExposedPorts: []string{"8200/tcp"},
			// The dev listener is IPv4 only, and the health check below dials the IPv4 host:
			// wait.ForHTTP would dial "localhost", which is ::1 first on some machines (OrbStack).
			WaitingFor: wait.ForLog("Development mode should NOT be used").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
	host, err := containerhost.Host(ctx, ctr)
	Expect(err).NotTo(HaveOccurred())
	port, err := ctr.MappedPort(ctx, "8200/tcp")
	Expect(err).NotTo(HaveOccurred())
	bao = fmt.Sprintf("http://%s:%s", host, port.Port())
	Eventually(func(g Gomega) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, bao+"/v1/sys/health", nil)
		g.Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		g.Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}).WithTimeout(time.Minute).Should(Succeed())

	// The transit engine, the key, and a decrypt-only token as koinon-api's policy would give it.
	root(ctx, "/v1/sys/mounts/transit", map[string]any{"type": "transit"})
	root(ctx, "/v1/transit/keys/"+testKey, map[string]any{})
	root(ctx, "/v1/sys/policies/acl/koinon-decrypt", map[string]any{
		"policy": `path "transit/decrypt/` + testKey + `" { capabilities = ["update"] }`,
	})
	// The key-admin Job's policy: update (never create, which would let encrypt create a missing
	// key) on encrypt and decrypt of any key in the mount, so a missing key stays missing.
	root(ctx, "/v1/sys/policies/acl/koinon-key-admin", map[string]any{
		"policy": `path "transit/encrypt/*" { capabilities = ["update"] }
path "transit/decrypt/*" { capabilities = ["update"] }`,
	})
	root(ctx, "/v1/auth/token/create", map[string]any{"id": adminToken, "policies": []string{"koinon-key-admin"}, "no_default_policy": true})
	root(ctx, "/v1/auth/token/create", map[string]any{"id": decryptToken, "policies": []string{"koinon-decrypt"}, "no_default_policy": true})
})

// root calls the dev server as root and fails the spec on an error.
func root(ctx context.Context, path string, body any) map[string]any {
	GinkgoHelper()
	out, status := call(ctx, rootToken, path, body)
	Expect(status).To(BeNumerically("<", 300), "%s: %v", path, out)
	return out
}

func call(ctx context.Context, token, path string, body any) (map[string]any, int) {
	GinkgoHelper()
	b, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bao+path, bytes.NewReader(b))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("X-Vault-Token", token)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	out := map[string]any{}
	if len(raw) > 0 {
		Expect(json.Unmarshal(raw, &out)).To(Succeed())
	}
	return out, resp.StatusCode
}
