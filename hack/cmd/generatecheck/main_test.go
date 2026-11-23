// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGeneratecheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "generatecheck")
}

var _ = Describe("missing", func() {
	found := func(string) (string, error) { return "/bin/x", nil }
	absent := func(string) (string, error) { return "", errors.New("not found") }

	It("is empty when node and the plugin are there", func() {
		root := GinkgoT().TempDir()
		p := filepath.Join(root, esPlugin)
		Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
		Expect(os.WriteFile(p, []byte("#!/usr/bin/env node\n"), 0o755)).To(Succeed())
		Expect(missing(root, found)).To(BeEmpty())
	})

	It("names the install command for each missing tool", func() {
		lines := missing(GinkgoT().TempDir(), absent)
		Expect(lines).To(HaveLen(2))
		Expect(lines[0]).To(ContainSubstring("corepack enable"))
		Expect(lines[1]).To(ContainSubstring("pnpm --dir web install --frozen-lockfile"))
	})

	It("exits 1 and prints one line per tool", func() {
		var out bytes.Buffer
		Expect(run(GinkgoT().TempDir(), found, &out)).To(Equal(1))
		Expect(bytes.Count(out.Bytes(), []byte("\n"))).To(Equal(1))
	})
})

var _ = Describe("the buf pin", func() {
	It("matches BUF_VERSION in hack/versions.env", func() {
		env, err := os.ReadFile("../../versions.env")
		Expect(err).NotTo(HaveOccurred())
		mod, err := os.ReadFile("../../tools/buf/go.mod")
		Expect(err).NotTo(HaveOccurred())
		m := regexp.MustCompile(`(?m)^BUF_VERSION=(\S+)$`).FindSubmatch(env)
		Expect(m).NotTo(BeNil())
		Expect(string(mod)).To(ContainSubstring("github.com/bufbuild/buf v" + string(m[1])))
	})
})
