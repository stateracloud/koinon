// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"bytes"
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/testing/s3test"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

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
		var err error
		s3, err = s3test.Start(sctx, func(f func(context.Context) error) { DeferCleanup(f) })
		Expect(err).NotTo(HaveOccurred())
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
