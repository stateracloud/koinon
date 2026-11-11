// SPDX-License-Identifier: Apache-2.0

package awskms_test

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

	"github.com/stateracloud/koinon/internal/secretstore/awskms"
	"github.com/stateracloud/koinon/internal/testing/containerhost"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func TestAWSKMS(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "AWS KMS Suite")
}

// image is the LocalStack the adapter is tested against. It does not check signatures or IAM
// policies, so rights and signing are covered against a fake (hardening_test.go) and AWS's SigV4
// test vectors (sigv4_test.go).
const image = containerhost.LocalStackImage

const region = "us-east-1"

// fakeCreds are what the suite signs LocalStack's requests with: obviously fake, never checked.
var fakeCreds = awskms.Credentials{
	AccessKeyID:     "koinon_test_not-a-real-key-id",
	SecretAccessKey: "koinon_test_not-a-real-secret",
	SessionToken:    "koinon_test_not-a-real-session-token",
}

var (
	// localstack is the LocalStack endpoint of this process.
	localstack string
	// testKey is a symmetric key's ARN; otherKey another key's ID.
	testKey, otherKey string
)

var _ = BeforeSuite(func(ctx SpecContext) {
	testenv.Require(testenv.Docker)
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			Env:          map[string]string{"SERVICES": "kms,sts", "SKIP_SSL_CERT_DOWNLOAD": "1", "DISABLE_EVENTS": "1"},
			ExposedPorts: []string{"4566/tcp"},
			WaitingFor:   wait.ForLog("Ready.").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	if ctr != nil {
		DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
	}
	Expect(err).NotTo(HaveOccurred())
	host, err := containerhost.Host(ctx, ctr)
	Expect(err).NotTo(HaveOccurred())
	port, err := ctr.MappedPort(ctx, "4566/tcp")
	Expect(err).NotTo(HaveOccurred())
	localstack = fmt.Sprintf("http://%s:%s", host, port.Port())

	testKey = createKey(ctx)["Arn"].(string)
	otherKey = createKey(ctx)["KeyId"].(string)
	kms(ctx, "CreateAlias", map[string]any{"AliasName": "alias/koinon-test", "TargetKeyId": testKey})
})

// createKey creates a symmetric encryption key and returns its metadata.
func createKey(ctx context.Context) map[string]any {
	GinkgoHelper()
	return kms(ctx, "CreateKey", map[string]any{"Description": "koinon test"})["KeyMetadata"].(map[string]any)
}

// kms calls LocalStack's KMS as its administrator (LocalStack checks no signature) and fails the
// spec on an error.
func kms(ctx context.Context, op string, body any) map[string]any {
	GinkgoHelper()
	b, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, localstack+"/", bytes.NewReader(b))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)
	awskms.Sign(req, b, fakeCreds.AccessKeyID, fakeCreds.SecretAccessKey, "", region, "kms", time.Now())
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK), "%s: %s", op, raw)
	out := map[string]any{}
	if len(raw) > 0 {
		Expect(json.Unmarshal(raw, &out)).To(Succeed())
	}
	return out
}
