// SPDX-License-Identifier: Apache-2.0

package guestfront_test

import (
	"os"
	"path/filepath"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The front embeds the upstream guest server from go.mod. hack/versions.env pins the same commit as
// ATE_ENV_VERSION, the commit whose guest.proto third_party/substrate holds: the two must not drift.
var _ = Describe("the upstream guest server pin", func() {
	It("is the commit ATE_ENV_VERSION in go.mod", func() {
		root := filepath.Join("..", "..")
		env, err := os.ReadFile(filepath.Join(root, "hack", "versions.env"))
		Expect(err).NotTo(HaveOccurred())
		m := regexp.MustCompile(`(?m)^ATE_ENV_VERSION=([0-9a-f]{40})$`).FindSubmatch(env)
		Expect(m).NotTo(BeNil(), "ATE_ENV_VERSION in hack/versions.env")
		mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
		Expect(err).NotTo(HaveOccurred())
		v := regexp.MustCompile(`(?m)^\s*github\.com/agent-substrate/env v\S+-([0-9a-f]{12})(\s|$)`).FindSubmatch(mod)
		Expect(v).NotTo(BeNil(), "a pseudo-version of github.com/agent-substrate/env in go.mod")
		Expect(string(m[1])).To(HavePrefix(string(v[1])))
	})
})
