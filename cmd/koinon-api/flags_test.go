// SPDX-License-Identifier: Apache-2.0

package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseFlags", func() {
	const db = "--database-url=postgres://koinon_test:not-a-real-password@localhost/koinon"

	It("runs the migrate subcommand without the OTLP token, as the chart's migrate Job does", func() {
		cfg, err := parseFlags([]string{db, "-wait", "2m", "migrate"})
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.migrateOnly()).To(BeTrue())
	})

	It("refuses to serve the OTLP receiver without its token", func() {
		_, err := parseFlags([]string{db})
		Expect(err).To(MatchError(ContainSubstring("--otlp-token-dir is required")))
	})

	It("serves without the receiver when it is turned off", func() {
		_, err := parseFlags([]string{db, "--otlp-addr="})
		Expect(err).NotTo(HaveOccurred())
	})
})
