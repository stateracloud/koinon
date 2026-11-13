// SPDX-License-Identifier: Apache-2.0

package runevidence_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/runevidence"
)

var _ = Describe("Sinks", func() {
	It("writes a directory sink and keeps only the base name", func(ctx SpecContext) {
		dir := GinkgoT().TempDir()
		s, err := runevidence.ParsePrefix(filepath.Join(dir, "x"), checkpoint.S3{})
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Put(ctx, "../run.jsonl", []byte("{}\n"))).To(Succeed())
		b, err := os.ReadFile(filepath.Join(dir, "x", "run.jsonl"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal("{}\n"))
	})

	It("refuses an s3 prefix with no bucket or no S3 settings", func() {
		_, err := runevidence.ParsePrefix("s3:///p", checkpoint.S3{})
		Expect(err).To(HaveOccurred())
		_, err = runevidence.ParsePrefix("s3://bucket/p", checkpoint.S3{})
		Expect(err).To(HaveOccurred())
	})
})
