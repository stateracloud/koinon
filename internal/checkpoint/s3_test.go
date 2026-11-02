// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/testing/containerhost"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// seaweedImage is the S3 server of the spike (ADR 0032): plain HTTP, path-style, no checksum
// trailers. Pulled through the Docker Hub prefix when TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX is set.
const seaweedImage = "chrislusf/seaweedfs:4.50"

const seaweedIdentities = `{"identities":[{"name":"test","credentials":[{"accessKey":"koinon_test_not-a-real-key","secretKey":"koinon_test_not-a-real-secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`

// The S3 path of the writer (checkpoint.S3: minio-go through kopia) against a real S3 server.
var _ = Describe("Writer on S3", Ordered, func() {
	var (
		s3     checkpoint.S3
		ctx    = context.Background()
		prefix = checkpoint.Prefix("0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000002")
		pw     = "koinon_test_not-a-real-token"
	)

	BeforeAll(func(sctx SpecContext) {
		testenv.Require(testenv.Docker)
		ctr, err := testcontainers.GenericContainer(sctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        seaweedImage,
				Cmd:          []string{"server", "-s3", "-s3.config=/etc/seaweedfs/s3.json", "-dir=/data", "-volume.max=8", "-master.volumeSizeLimitMB=64"},
				ExposedPorts: []string{"8333/tcp"},
				Files: []testcontainers.ContainerFile{{
					Reader: strings.NewReader(seaweedIdentities), ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644,
				}},
				WaitingFor: wait.ForListeningPort("8333/tcp").WithStartupTimeout(2 * time.Minute),
			},
			Started: true,
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
		// SeaweedFS listens on IPv4 only (#290): dial the IPv4 loopback.
		host, err := containerhost.Host(sctx, ctr)
		Expect(err).NotTo(HaveOccurred())
		port, err := ctr.MappedPort(sctx, "8333/tcp")
		Expect(err).NotTo(HaveOccurred())
		s3 = checkpoint.S3{
			Endpoint: fmt.Sprintf("http://%s:%s", host, port.Port()), Region: "us-east-1", Bucket: "koinon-homes",
			AccessKeyID: "koinon_test_not-a-real-key", SecretAccessKey: "koinon_test_not-a-real-secret",
		}
		Expect(s3.Validate()).To(Succeed())

		mc, err := minio.New(fmt.Sprintf("%s:%s", host, port.Port()), &minio.Options{Creds: credentials.NewStaticV4(s3.AccessKeyID, s3.SecretAccessKey, "")})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			return mc.MakeBucket(sctx, s3.Bucket, minio.MakeBucketOptions{Region: s3.Region})
		}, time.Minute, time.Second).Should(Succeed())
	})

	It("backs up, restores and verifies through the S3 client", func() {
		scratch := GinkgoT().TempDir()
		w := &checkpoint.Writer{Storage: s3, Scratch: scratch}
		r, err := w.Backup(ctx, prefix, pw, makeTar(
			entry{name: "./notes/", typ: '5', mode: 0o755},
			entry{name: "./notes/a.md", body: "first note"},
			entry{name: "./big.bin", body: strings.Repeat("0123456789abcdef", 1<<16)},
		), checkpoint.Meta{CheckpointID: "c1", Generation: "g1", Epoch: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.SnapshotID).NotTo(BeEmpty())

		var out bytes.Buffer
		Expect(w.Restore(ctx, prefix, pw, r.SnapshotID, &out)).To(Succeed())
		got := readTar(out.Bytes())
		Expect(got).To(HaveKeyWithValue("notes/a.md", "first note"))
		Expect(got["big.bin"]).To(HaveLen(16 << 16))
		Expect(w.Verify(ctx, prefix, pw, r.SnapshotID)).To(Succeed())

		By("the wrong password does not open the repository")
		Expect(w.Restore(ctx, prefix, "koinon_test_wrong", r.SnapshotID, &bytes.Buffer{})).NotTo(Succeed())
	})
})
