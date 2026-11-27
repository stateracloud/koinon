// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/preflight"
)

var _ = Describe("SharedFilesystemsUnder", func() {
	It("reports a missing /var/lib/ate", func() {
		shared, missing, err := preflight.SharedFilesystemsUnder(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(missing).To(BeTrue())
		Expect(shared).To(BeEmpty())
	})

	It("reports a directory on the same device as the root and kubelet", func() {
		root := GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(root, "var/lib/ate"), 0o750)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(root, "var/lib/kubelet"), 0o750)).To(Succeed())
		shared, missing, err := preflight.SharedFilesystemsUnder(root)
		Expect(err).NotTo(HaveOccurred())
		Expect(missing).To(BeFalse())
		Expect(shared).To(Equal([]string{"root", "kubelet"}))
	})

	It("compares with the root only when the node has no kubelet directory", func() {
		root := GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(root, "var/lib/ate"), 0o750)).To(Succeed())
		shared, _, err := preflight.SharedFilesystemsUnder(root)
		Expect(err).NotTo(HaveOccurred())
		Expect(shared).To(Equal([]string{"root"}))
	})
})
