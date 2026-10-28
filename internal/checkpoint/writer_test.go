// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
