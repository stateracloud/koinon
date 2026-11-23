// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// licenceNotices are the files under /usr/share/doc/koinon-tools/ the image's own build-time
// check already requires to be present and non-empty (harnesses/opencode/Dockerfile): every
// licence text a redistributed binary needs, plus the GPL/LGPL written source offer (#63).
var licenceNotices = []string{
	"fd/LICENSE-MIT", "fd/LICENSE-APACHE",
	"ripgrep/UNLICENSE", "ripgrep/LICENSE-MIT", "ripgrep/COPYING",
	"yq/LICENSE", "ast-grep/LICENSE", "duckdb/LICENSE", "opencode/LICENSE", "bun/LICENSE.md",
	"THIRD_PARTY_NOTICES.md",
	"GPL-SOURCE-OFFER.txt",
}

// describeLicenceNotices registers, for one harness, that every licence notice and the GPL
// source offer are readable by the sandbox user, not just by root. Both the licences stage's
// ADD (a remote-URL ADD with no --chmod defaults to 0600, root-only) and the build-time check
// that confirms the files exist run as root, so neither catches a notice a non-root reader
// cannot open; only a read as the sandbox user does.
func describeLicenceNotices(f definition) bool {
	return Describe(f.name+" licence notices", func() {
		BeforeEach(func() {
			testenv.Require(testenv.Docker)
			ensureImage(f)
		})

		It("lets the sandbox user read every licence notice and the GPL source offer", func(ctx SpecContext) {
			sb := newSandbox(f.image())
			DeferCleanup(func() {
				sb.remove(context.Background())
			})
			sb.start(ctx)
			for _, name := range licenceNotices {
				path := "/usr/share/doc/koinon-tools/" + name
				out, stderr, code := sb.try(ctx, "cat", path)
				Expect(code).To(BeZero(), "reading %s as the sandbox user: %s", path, stderr)
				Expect(out).NotTo(BeEmpty(), "%s is empty", path)
			}
		}, SpecTimeout(time.Minute))
	})
}

var _ = func() bool {
	for _, f := range harnesses {
		describeLicenceNotices(f)
	}
	return true
}()
