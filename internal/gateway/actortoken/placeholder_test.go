// SPDX-License-Identifier: Apache-2.0

package actortoken_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// The OpenCode entry script repeats the placeholder (issue #1489): a shell script cannot import it.
var _ = It("is the model key the OpenCode harness sends when the sandbox has none", func() {
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "harnesses", "opencode", "koinon-opencode"))
	Expect(err).NotTo(HaveOccurred())
	Expect(string(script)).To(ContainSubstring(`export KOINON_MODEL_KEY="${OPENAI_API_KEY:-` + actortoken.Placeholder + `}"`))
})
