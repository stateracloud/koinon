// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/execution"
)

// The home drill of issue #1090 on the fake: the two mechanisms of the home, in order. The durable
// directory keeps the home through suspend and resume. The kopia checkpoint keeps it when the
// sandbox is deleted, and a restore with a wrong digest changes nothing.
var _ = Describe("the home of a Substrate sandbox (issue #1090)", func() {
	const (
		pw     = "koinon_test_not-a-real-token"
		prefix = "orgs/o/homes/a/"
	)

	homeTar := func(files map[string]string) io.Reader {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			Expect(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})).To(Succeed())
			_, err := tw.Write([]byte(body))
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(tw.Close()).To(Succeed())
		return &buf
	}

	// backupTar reads the home the way the checkpointer does: tar over exec.
	backupTar := func(ctx context.Context, d execution.Compute, name string) []byte {
		p, err := d.Exec(ctx, ws, name, execution.ExecRequest{Command: []string{"tar", "-C", "/sandbox", "-cf", "-", "."}})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)
		out, err := io.ReadAll(p.Stdout())
		Expect(err).NotTo(HaveOccurred())
		code, err := p.Wait()
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		return out
	}

	It("keeps the home through suspend, and restores it from a checkpoint after a delete, only with the right digest", func(ctx SpecContext) {
		d, _ := newDriver()
		ready(ctx, d, "old")
		Expect(d.Upload(ctx, ws, "old", "/sandbox", homeTar(map[string]string{"notes/a.md": "first", "b.txt": "second"}))).To(Succeed())

		// Suspend and resume: the files stay.
		_, err := d.SuspendSandbox(ctx, ws, "old")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.ResumeSandbox(ctx, ws, "old")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.WaitPhase(ctx, ws, "old", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())
		stream := backupTar(ctx, d, "old")
		Expect(stream).NotTo(BeEmpty())

		w := &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: GinkgoT().TempDir()}
		res, err := w.Backup(ctx, prefix, pw, bytes.NewReader(stream), checkpoint.Meta{CheckpointID: "c1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(checkpoint.ValidDigest(res.Digest)).To(BeTrue())

		// The sandbox is deleted: the home is gone.
		_, err = d.DeleteSandbox(ctx, ws, "old")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := d.GetSandbox(ctx, ws, "old")
			return err
		}, 2*time.Second, 10*time.Millisecond).Should(HaveOccurred())

		ready(ctx, d, "new")
		Expect(backupTar(ctx, d, "new")).To(HaveLen(1024), "a new sandbox has an empty home (two zero blocks)")

		// A restore with a wrong digest fails and writes nothing.
		var wrong bytes.Buffer
		err = w.Restore(ctx, prefix, pw, res.SnapshotID, "sha256:"+string(bytes.Repeat([]byte("0"), 64)), &wrong)
		Expect(err).To(MatchError(checkpoint.ErrDigestMismatch))
		Expect(wrong.Len()).To(BeZero())
		Expect(backupTar(ctx, d, "new")).To(HaveLen(1024), "the new home is still empty")

		// A restore with the digest of the checkpoint brings the files back.
		var right bytes.Buffer
		Expect(w.Restore(ctx, prefix, pw, res.SnapshotID, res.Digest, &right)).To(Succeed())
		Expect(d.Upload(ctx, ws, "new", "/sandbox", &right)).To(Succeed())
		again := backupTar(ctx, d, "new")
		res2, err := w.Backup(ctx, prefix, pw, bytes.NewReader(again), checkpoint.Meta{CheckpointID: "c2"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Digest).To(Equal(res.Digest), "the restored home has the digest of the checkpoint")
	}, SpecTimeout(60*time.Second))
})
