// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// s3Ready reports nil once the server serves the calls the writer makes: it creates the bucket
// when missing, then writes, reads and deletes a probe object (the write path needs the volume
// servers, which come up after the S3 port).
func s3Ready(ctx context.Context, mc *minio.Client, bucket, region string) error {
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		if resp := minio.ToErrorResponse(err); resp.Code != "BucketAlreadyOwnedByYou" && resp.Code != "BucketAlreadyExists" {
			return err
		}
	}
	const probe = ".readiness-probe"
	if _, err := mc.PutObject(ctx, bucket, probe, strings.NewReader("ok"), 2, minio.PutObjectOptions{}); err != nil {
		return err
	}
	obj, err := mc.GetObject(ctx, bucket, probe, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = obj.Close() }()
	if _, err := io.Copy(io.Discard, obj); err != nil {
		return err
	}
	return mc.RemoveObject(ctx, bucket, probe, minio.RemoveObjectOptions{})
}

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
		// SeaweedFS listens on IPv4 only (old issue 290): dial the IPv4 loopback.
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
		// The port answers before the S3 gateway, the filer and the volume servers are ready
		// (#37). Wait on real S3 calls succeeding, a bucket create and an object
		// write/read/delete, not on the bare port.
		// Each attempt has its own deadline, so a hung call (a half-open connection after a reset)
		// cannot stall Eventually past its window.
		Eventually(func() error {
			actx, cancel := context.WithTimeout(sctx, 10*time.Second)
			defer cancel()
			return s3Ready(actx, mc, s3.Bucket, s3.Region)
		}, 2*time.Minute, time.Second).Should(Succeed())
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

	It("lists an agent's later epoch folders in one delimited listing, past a gap and without epoch 0's blobs (#262)", func() {
		w := &checkpoint.Writer{Storage: s3, Scratch: GinkgoT().TempDir()}
		org, agent := "0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000003"
		held, err := w.HeldEpochs(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(held).To(BeEmpty())
		for _, n := range []int32{0, 1, 3, 12} {
			_, err := w.Backup(ctx, checkpoint.EpochPrefix(org, agent, n), pw, makeTar(entry{name: "./notes/a.md", body: "note"}), checkpoint.Meta{CheckpointID: "c"})
			Expect(err).NotTo(HaveOccurred())
		}
		held, err = w.HeldEpochs(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(held).To(Equal([]int32{1, 3, 12}))
	})
})
