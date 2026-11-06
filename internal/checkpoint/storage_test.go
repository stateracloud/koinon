// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
)

var _ = Describe("the S3 access key's fingerprint", func() {
	It("names the key ID without showing it, and tells two keys apart", func() {
		a := checkpoint.S3{AccessKeyID: "koinon_test_not-a-real-key", SecretAccessKey: "koinon_test_not-a-real-secret"}
		b := checkpoint.S3{AccessKeyID: "koinon_test_not-a-real-key-2", SecretAccessKey: "koinon_test_not-a-real-secret"}
		Expect(a.AccessKeyFingerprint()).To(MatchRegexp(`^sha256:[0-9a-f]{16}$`))
		Expect(a.AccessKeyFingerprint()).To(Equal(a.AccessKeyFingerprint()))
		Expect(a.AccessKeyFingerprint()).NotTo(Equal(b.AccessKeyFingerprint()))
		Expect(a.AccessKeyFingerprint()).NotTo(ContainSubstring("not-a-real"))
		Expect(checkpoint.S3{}.AccessKeyFingerprint()).To(BeEmpty())
	})
})
