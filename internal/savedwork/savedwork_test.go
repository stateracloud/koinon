// SPDX-License-Identifier: Apache-2.0

package savedwork_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

func sample() savedwork.Work {
	return savedwork.Work{
		Manifest: savedwork.Manifest{
			Repo: "acme/widgets", Base: strings.Repeat("a", 40),
			Files: []savedwork.File{
				{Path: "a/b.go", Status: "M", Mode: "100644", Size: 5, Added: 2, Deleted: 1},
				{Path: "gone.txt", Status: "D", Deleted: 3},
			},
		},
		Content: map[string][]byte{"a/b.go": []byte("hello")},
	}
}

var _ = Describe("saved work", func() {
	It("packs and unpacks, and seals under a binding to its row", func() {
		ring := secretstoretest.Keyring()
		env, err := savedwork.Seal(ring, "org-1", "row-1", sample())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(env.Ciphertext)).NotTo(ContainSubstring("hello"))
		w, err := savedwork.Open(ring, "org-1", "row-1", env)
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Manifest).To(Equal(sample().Manifest))
		Expect(string(w.Content["a/b.go"])).To(Equal("hello"))

		_, err = savedwork.Open(ring, "org-1", "row-2", env)
		Expect(err).To(HaveOccurred(), "another row")
		_, err = savedwork.Open(ring, "org-2", "row-1", env)
		Expect(err).To(HaveOccurred(), "another org")
	})

	It("totals the diffstat", func() {
		m := sample().Manifest
		added, deleted := m.Totals()
		Expect(added).To(Equal(2))
		Expect(deleted).To(Equal(4))
		Expect(m.Bytes()).To(BeEquivalentTo(5))
	})

	DescribeTable("rejects a manifest that could write where it should not",
		func(mutate func(*savedwork.Work)) {
			w := sample()
			mutate(&w)
			_, err := savedwork.Pack(w)
			Expect(err).To(HaveOccurred())
		},
		Entry("a path out of the repository", func(w *savedwork.Work) { w.Manifest.Files[0].Path = "../etc/passwd" }),
		Entry("an absolute path", func(w *savedwork.Work) { w.Manifest.Files[0].Path = "/etc/passwd" }),
		Entry("a .git path", func(w *savedwork.Work) { w.Manifest.Files[0].Path = "x/.GIT/config" }),
		Entry("a path with a control character", func(w *savedwork.Work) { w.Manifest.Files[0].Path = "a\nb" }),
		Entry("a symbolic link mode", func(w *savedwork.Work) { w.Manifest.Files[0].Mode = "120000" }),
		Entry("a missing content", func(w *savedwork.Work) { delete(w.Content, "a/b.go") }),
		Entry("a size that differs", func(w *savedwork.Work) { w.Manifest.Files[0].Size = 6 }),
		Entry("the same path twice", func(w *savedwork.Work) { w.Manifest.Files = append(w.Manifest.Files, w.Manifest.Files[0]) }),
		Entry("over 50 MB", func(w *savedwork.Work) {
			w.Manifest.Files[0].Size = savedwork.MaxBytes + 1
			w.Content["a/b.go"] = make([]byte, savedwork.MaxBytes+1)
		}),
		Entry("more notes than allowed", func(w *savedwork.Work) {
			for range savedwork.MaxNotes + 1 {
				w.Manifest.Skipped = append(w.Manifest.Skipped, "x")
			}
		}),
		Entry("a note that is too long", func(w *savedwork.Work) { w.Manifest.Skipped = []string{strings.Repeat("x", savedwork.MaxNoteBytes+1)} }),
		Entry("no files", func(w *savedwork.Work) { w.Manifest.Files = nil }),
	)

	It("names the branch koinon/wip/<task>-<time>, and accepts only such names", func() {
		b := savedwork.BranchName("0192a3bc-1111-7222-8333-444455556666", time.Date(2026, 10, 24, 10, 15, 30, 0, time.UTC))
		Expect(b).To(Equal("koinon/wip/0192a3bc-20261024T101530Z"))
		Expect(savedwork.ValidBranch(b)).To(BeTrue())
		for _, bad := range []string{"main", "koinon/wip/", "koinon/wip/../x", "koinon/wip/a b", "koinon/wipx/a", "refs/heads/main", "koinon/wip/a.lock"} {
			Expect(savedwork.ValidBranch(bad)).To(BeFalse(), bad)
		}
	})

	It("reads push arguments strictly", func() {
		args := savedwork.PushArgsJSON("id", "acme/widgets", "base", "koinon/wip/x-1", strings.Repeat("a", 64), strings.Repeat("b", 64))
		Expect(args).To(Equal(`{"base":"base","branch":"koinon/wip/x-1","bundle_sha256":"` + strings.Repeat("a", 64) +
			`","manifest_sha256":"` + strings.Repeat("b", 64) + `","repo":"acme/widgets","saved_work_id":"id"}`))
		a, err := savedwork.DecodePushArgs(args)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Branch).To(Equal("koinon/wip/x-1"))
		bad := func(branch, extra string) string {
			return `{"base":"b","branch":"` + branch + `","bundle_sha256":"` + strings.Repeat("a", 64) + `","manifest_sha256":"` + strings.Repeat("b", 64) + `","repo":"r","saved_work_id":"i"` + extra + `}`
		}
		_, err = savedwork.DecodePushArgs(bad("main", ""))
		Expect(err).To(HaveOccurred())
		_, err = savedwork.DecodePushArgs(bad("koinon/wip/x", `,"extra":1`))
		Expect(err).To(HaveOccurred())
		_, err = savedwork.DecodePushArgs(`{"base":"b","branch":"koinon/wip/x","repo":"r","saved_work_id":"i"}`)
		Expect(err).To(HaveOccurred(), "the hashes are required")
	})
})
