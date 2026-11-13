// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"

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

	It("writes nothing to the bucket in restore-only mode (#940)", func() {
		bucket := GinkgoT().TempDir()
		src := &checkpoint.Writer{Storage: checkpoint.Dir(bucket), Scratch: GinkgoT().TempDir()}
		pw := "koinon_test_not-a-real-token"
		r, err := src.Backup(ctx, prefix, pw, home(), checkpoint.Meta{CheckpointID: "c1"})
		Expect(err).NotTo(HaveOccurred())

		state := func() map[string]string {
			out := map[string]string{}
			Expect(filepath.WalkDir(bucket, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				fi, err := d.Info()
				Expect(err).NotTo(HaveOccurred())
				out[p] = fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano())
				return nil
			})).To(Succeed())
			return out
		}
		before := state()

		ro := &checkpoint.Writer{Storage: checkpoint.Dir(bucket), Scratch: GinkgoT().TempDir(), ReadOnly: true}
		var out bytes.Buffer
		Expect(ro.Restore(ctx, prefix, pw, r.SnapshotID, &out)).To(Succeed())
		Expect(readTar(out.Bytes())).To(HaveKeyWithValue("notes/a.md", "first note"))
		Expect(ro.Verify(ctx, prefix, pw, r.SnapshotID)).To(Succeed())
		snaps, err := ro.Snapshots(ctx, prefix, pw)
		Expect(err).NotTo(HaveOccurred())
		Expect(snaps).To(HaveLen(1))
		Expect(state()).To(Equal(before), "no blob was put, rewritten or deleted")

		_, err = ro.Backup(ctx, prefix, pw, home(), checkpoint.Meta{CheckpointID: "c2"})
		Expect(err).To(MatchError(ContainSubstring(checkpoint.ReadOnlyError)))
		Expect(state()).To(Equal(before))
	})

	It("does not open a repository with another password", func() {
		_, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{})
		Expect(err).NotTo(HaveOccurred())
		var out bytes.Buffer
		Expect(w.Restore(ctx, prefix, "koinon_test_wrong", "", &out)).NotTo(Succeed())
		_, err = w.Backup(ctx, prefix, "koinon_test_wrong", home(), checkpoint.Meta{})
		Expect(err).To(HaveOccurred())
	})

	It("lists the agent's later epoch folders that hold anything, past a gap (#262)", func() {
		org, agent := "0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000009"
		held, err := w.HeldEpochs(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(held).To(BeEmpty(), "nothing written yet")
		for _, n := range []int32{0, 1, 3, 12} {
			_, err := w.Backup(ctx, checkpoint.EpochPrefix(org, agent, n), "koinon_test_not-a-real-token", home(), checkpoint.Meta{CheckpointID: "c"})
			Expect(err).NotTo(HaveOccurred())
		}
		held, err = w.HeldEpochs(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(held).To(Equal([]int32{1, 3, 12}), "sorted, r2/ absent, epoch 0's own blobs not an epoch")
	})

	It("tells a prefix that holds another password's repository apart, and creates nothing over it (#262)", func() {
		held, err := w.HoldsRepository(ctx, prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(held).To(BeFalse(), "nothing written yet")
		r1, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{CheckpointID: "c1"})
		Expect(err).NotTo(HaveOccurred())
		held, err = w.HoldsRepository(ctx, prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(held).To(BeTrue())

		_, err = w.Backup(ctx, prefix, "koinon_test_wrong", home(), checkpoint.Meta{CheckpointID: "c2"})
		Expect(err).To(MatchError(checkpoint.ErrPrefixTaken))
		Expect(err.Error()).To(ContainSubstring(prefix))
		Expect(checkpoint.TakenPrefix(fmt.Errorf("wrapped: %w", err), prefix)).To(BeTrue())
		Expect(checkpoint.TakenPrefix(err, prefix+"r1/")).To(BeFalse(), "names the prefix it was")
		_, err = w.Snapshots(ctx, prefix, "koinon_test_wrong")
		Expect(err).To(MatchError(checkpoint.ErrPrefixTaken))
		snaps, err := w.Snapshots(ctx, prefix, "koinon_test_not-a-real-token")
		Expect(err).NotTo(HaveOccurred())
		Expect(snaps).To(HaveLen(1), "the other password wrote nothing")
		Expect(snaps[0].ID).To(Equal(r1.SnapshotID))
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

	// koinon never runs kopia maintenance, so these are all the blobs a repository holds.
	It("names every blob a backup, restore and verify write with a prefix the sweep lists (a kopia bump that adds one fails here)", func() {
		pass := "koinon_test_not-a-real-token"
		for i := range 3 {
			_, err := w.Backup(ctx, prefix, pass, home(entry{name: fmt.Sprintf("./notes/%d.md", i), body: strings.Repeat("x", i+1)}), checkpoint.Meta{CheckpointID: fmt.Sprintf("c%d", i)})
			Expect(err).NotTo(HaveOccurred())
		}
		var out bytes.Buffer
		Expect(w.Restore(ctx, prefix, pass, "", &out)).To(Succeed())
		last, err := w.Backup(ctx, prefix, pass, home(), checkpoint.Meta{CheckpointID: "last"})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Verify(ctx, prefix, pass, last.SnapshotID)).To(Succeed())

		ids, err := w.BlobIDs(ctx, prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).NotTo(BeEmpty())
		for _, id := range ids {
			listed := false
			for _, p := range checkpoint.KopiaBlobPrefixes {
				listed = listed || strings.HasPrefix(string(id), string(p))
			}
			Expect(listed).To(BeTrue(), "blob %q has a prefix the sweep does not list: add it to kopiaBlobPrefixes", id)
		}
		Expect(w.DeleteRepository(ctx, prefix)).To(Equal(len(ids)))
		left, err := w.BlobIDs(ctx, prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(left).To(BeEmpty())
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

	// checkpoints resync (ADR 0085 decision 17) rebuilds home_checkpoints from these listings.
	Describe("Snapshots", func() {
		const pass = "koinon_test_not-a-real-token"

		It("lists every complete snapshot, oldest first, with what its tags say", func() {
			r1, err := w.Backup(ctx, prefix, pass, home(), checkpoint.Meta{CheckpointID: "c1", Generation: "g1", Epoch: 3, Reason: "stop", Sandbox: "sb-1"})
			Expect(err).NotTo(HaveOccurred())
			r2, err := w.Backup(ctx, prefix, pass, home(entry{name: "./notes/b.md", body: "second"}), checkpoint.Meta{CheckpointID: "c2", Generation: "g1"})
			Expect(err).NotTo(HaveOccurred())

			got, err := w.Snapshots(ctx, prefix, pass)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(2))
			Expect(got[0].ID).To(Equal(r1.SnapshotID))
			Expect(got[1].ID).To(Equal(r2.SnapshotID))
			Expect(got[0].MetaErr).NotTo(HaveOccurred())
			Expect(got[0].Meta).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
				"CheckpointID": Equal("c1"), "Generation": Equal("g1"), "Epoch": BeEquivalentTo(3), "Reason": Equal("stop"), "Sandbox": Equal("sb-1"),
			}))
			Expect(got[1].Meta.Reason).To(BeEmpty(), "an empty reason is not stamped")
			Expect(got[0].Bytes).To(Equal(r1.Bytes), "the size of its files, as the backup counted them")
			Expect(got[0].Start).NotTo(BeZero())
			Expect(got[0].End).NotTo(BeTemporally("<", got[0].Start))
			Expect(got[1].Start).NotTo(BeTemporally("<", got[0].Start))
		})

		It("tells a prefix with no repository apart from an empty listing, and creates nothing", func() {
			_, err := w.Snapshots(ctx, prefix, pass)
			Expect(err).To(MatchError(checkpoint.ErrNoRepositoryInBucket))
			_, err = w.Snapshots(ctx, prefix, pass)
			Expect(err).To(MatchError(checkpoint.ErrNoRepositoryInBucket), "the first listing created no repository")
		})

		It("does not list a repository with another password", func() {
			_, err := w.Backup(ctx, prefix, pass, home(), checkpoint.Meta{})
			Expect(err).NotTo(HaveOccurred())
			_, err = w.Snapshots(ctx, prefix, "koinon_test_wrong")
			Expect(err).To(HaveOccurred())
			Expect(err).NotTo(MatchError(checkpoint.ErrNoRepositoryInBucket))
		})

		It("lists one epoch's repository only, and a deleted one as gone", func() {
			org, agent := "0198a000-0000-7000-8000-000000000001", "0198a000-0000-7000-8000-000000000002"
			e1 := checkpoint.EpochPrefix(org, agent, 1)
			_, err := w.Backup(ctx, prefix, pass, home(), checkpoint.Meta{CheckpointID: "c0"})
			Expect(err).NotTo(HaveOccurred())
			s1, err := w.Backup(ctx, e1, pass+"-1", home(), checkpoint.Meta{CheckpointID: "c1"})
			Expect(err).NotTo(HaveOccurred())
			got, err := w.Snapshots(ctx, e1, pass+"-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1))
			Expect(got[0].ID).To(Equal(s1.SnapshotID))
			Expect(got[0].Meta.CheckpointID).To(Equal("c1"))

			_, err = w.DeleteRepository(ctx, prefix)
			Expect(err).NotTo(HaveOccurred())
			_, err = w.Snapshots(ctx, prefix, pass)
			Expect(err).To(MatchError(checkpoint.ErrNoRepositoryInBucket))
		})

		It("leaves out a snapshot kopia's retention deleted", func() {
			var last checkpoint.Result
			for i := range checkpoint.Retention.Latest + 2 {
				var err error
				last, err = w.Backup(ctx, prefix, pass, home(entry{name: "./notes/n.md", body: strings.Repeat("x", i+1)}), checkpoint.Meta{CheckpointID: fmt.Sprintf("c%d", i)})
				Expect(err).NotTo(HaveOccurred())
			}
			got, err := w.Snapshots(ctx, prefix, pass)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(got)).To(BeNumerically("<", checkpoint.Retention.Latest+2))
			Expect(got[len(got)-1].ID).To(Equal(last.SnapshotID))
		})
	})

	It("writes no snapshot when Ready refuses the staged home", func() {
		bad := errors.New("tar in the sandbox exited with 2")
		calls := 0
		_, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{Ready: func() error { calls++; return bad }})
		Expect(err).To(MatchError(bad))
		Expect(calls).To(Equal(1))
		_, err = w.Snapshots(ctx, prefix, "koinon_test_not-a-real-token")
		Expect(err).To(MatchError(checkpoint.ErrNoRepositoryInBucket), "not even the repository was written")

		r, err := w.Backup(ctx, prefix, "koinon_test_not-a-real-token", home(), checkpoint.Meta{Ready: func() error { return nil }})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Verify(ctx, prefix, "koinon_test_not-a-real-token", r.SnapshotID)).To(Succeed())
	})
})
