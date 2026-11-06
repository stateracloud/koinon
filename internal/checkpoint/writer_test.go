// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
)

func readTar(b []byte) map[string]string {
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		Expect(err).NotTo(HaveOccurred())
		body, err := io.ReadAll(tr)
		Expect(err).NotTo(HaveOccurred())
		out[strings.TrimSuffix(h.Name, "/")] = string(body)
	}
}

var _ = Describe("Writer", func() {
	var (
		w      *checkpoint.Writer
		prefix = checkpoint.Prefix("0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000002")
		ctx    = context.Background()
	)
	BeforeEach(func() {
		scratch := GinkgoT().TempDir()
		w = &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: scratch}
		DeferCleanup(func() {
			ents, _ := os.ReadDir(scratch)
			Expect(ents).To(BeEmpty(), "the writer leaves nothing in its scratch directory")
		})
	})

	home := func(extra ...entry) *bytes.Buffer {
		return makeTar(append([]entry{
			{name: "./", typ: tar.TypeDir, mode: 0o755},
			{name: "./notes/", typ: tar.TypeDir, mode: 0o755},
			{name: "./notes/a.md", body: "first note"},
			{name: "./.local/share/opencode/s1.json", body: `{"session":"s1"}`},
		}, extra...)...)
	}

	It("saves a home and restores it, newest first", func() {
		r1, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{CheckpointID: "c1", Generation: "g1", Epoch: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(r1.SnapshotID).NotTo(BeEmpty())
		Expect(r1.Files).To(BeEquivalentTo(2))

		r2, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(entry{name: "./notes/b.md", body: "second"}), checkpoint.Meta{CheckpointID: "c2"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r2.SnapshotID).NotTo(Equal(r1.SnapshotID))

		var newest, older bytes.Buffer
		Expect(w.Restore(ctx, prefix, "koinon_test_not-a-real-token", "", &newest)).To(Succeed())
		Expect(readTar(newest.Bytes())).To(HaveKeyWithValue("notes/b.md", "second"))
		Expect(w.Restore(ctx, prefix, "koinon_test_not-a-real-token", r1.SnapshotID, &older)).To(Succeed())
		got := readTar(older.Bytes())
		Expect(got).To(HaveKeyWithValue("notes/a.md", "first note"))
		Expect(got).NotTo(HaveKey("notes/b.md"))
		Expect(w.Verify(ctx, prefix, "koinon_test_not-a-real-token", r2.SnapshotID)).To(Succeed())
	})

	It("does not open a repository with another password", func() {
		_, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{})
		Expect(err).NotTo(HaveOccurred())
		var out bytes.Buffer
		Expect(w.Restore(ctx, prefix, "koinon_test_wrong", "", &out)).NotTo(Succeed())
		_, err = w.Backup(ctx, prefix, "koinon_test_wrong", home(), checkpoint.Meta{})
		Expect(err).To(HaveOccurred())
	})

	It("keeps agents' repositories apart", func() {
		other := checkpoint.Prefix("0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000003")
		_, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{})
		Expect(err).NotTo(HaveOccurred())
		var out bytes.Buffer
		Expect(w.Restore(ctx, other, "koinon_test_not-a-real-token", "", &out)).NotTo(Succeed())
	})

	It("deletes a retired repository's blobs and leaves the later epoch under it readable", func() {
		org, agent := "0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000002"
		e1 := checkpoint.EpochPrefix(org, agent, 1)
		Expect(e1).To(HavePrefix(prefix), "epoch 1 is nested under epoch 0")
		_, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{CheckpointID: "c0"})
		Expect(err).NotTo(HaveOccurred())
		s1, err := w.Backup(ctx, e1, "koinon_test_not-a-real-token-1", home(entry{name: "./notes/b.md", body: "epoch one"}), checkpoint.Meta{CheckpointID: "c1"})
		Expect(err).NotTo(HaveOccurred())

		n, err := w.DeleteRepository(ctx, prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">", 0))
		var gone bytes.Buffer
		Expect(w.Restore(ctx, prefix, "koinon_test_not-a-real-token", "", &gone)).NotTo(Succeed(), "epoch 0 is deleted")

		By("epoch 1 still opens and restores")
		var out bytes.Buffer
		Expect(w.Restore(ctx, e1, "koinon_test_not-a-real-token-1", "", &out)).To(Succeed())
		Expect(readTar(out.Bytes())).To(HaveKeyWithValue("notes/b.md", "epoch one"))
		Expect(w.Verify(ctx, e1, "koinon_test_not-a-real-token-1", s1.SnapshotID)).To(Succeed())

		By("nothing of epoch 0 is left beside the r1 folder")
		dir := filepath.Join(string(w.Storage.(checkpoint.Dir)), filepath.FromSlash(prefix))
		var left []string
		Expect(filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && path == filepath.Join(dir, "r1"):
				return filepath.SkipDir
			case !d.IsDir() && d.Name() != ".shards": // .shards: the directory backend's layout, not a blob
				left = append(left, path)
			}
			return nil
		})).To(Succeed())
		Expect(left).To(BeEmpty())

		By("deleting again, or a repository never written, deletes nothing")
		Expect(w.DeleteRepository(ctx, prefix)).To(BeZero())
		Expect(w.DeleteRepository(ctx, checkpoint.Prefix(org, "0198a000-0000-7000-8000-000000000009"))).To(BeZero())
		_, err = w.DeleteRepository(ctx, "orgs/x")
		Expect(err).To(MatchError(ContainSubstring("not a repository prefix")))
	})

	It("restores from a repository that has no snapshot as an error", func() {
		var out bytes.Buffer
		Expect(w.Restore(ctx, prefix, "koinon_test_not-a-real-token", "", &out)).NotTo(Succeed())
	})

	It("refuses a home over the limit and leaves no snapshot", func() {
		w.Limits = checkpoint.Limits{MaxBytes: 4}
		_, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{})
		Expect(err).To(MatchError(checkpoint.ErrTooLarge))
	})
})
