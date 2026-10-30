// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-agentd")
}

var _ = Describe("OTLP exporter settings", func() {
	base := []string{"--database-url", "postgres://x@db/koinon", "--openshell-address", "o:1", "--gateway-url", "http://g:8080"}
	tokenFile := func(content string) string {
		p := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(p, []byte(content), 0o600)).To(Succeed())
		return p
	}

	// otlp returns the base flags with the two OTLP flags (a flag with an empty value is left out).
	otlp := func(endpoint, file string) []string {
		args := append([]string{}, base...)
		if endpoint != "" {
			args = append(args, "--otlp-endpoint", endpoint)
		}
		if file != "" {
			args = append(args, "--otlp-token-file", file)
		}
		return args
	}
	const endpoint = "http://koinon-api.koinon.svc"

	It("is off by default", func() {
		c, err := parseFlags(base)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.otlpEndpoint).To(BeEmpty())
	})

	It("takes the endpoint and the token file, from flags or the environment", func() {
		tok := tokenFile("koinon_test_not-a-real-token-agentd-0001\n")
		c, err := parseFlags(otlp(endpoint+":4318", tok))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.otlpTokenFile).To(Equal(tok))

		GinkgoT().Setenv("KOINON_OTLP_ENDPOINT", endpoint+":4318")
		GinkgoT().Setenv("KOINON_OTLP_TOKEN_FILE", tok)
		c, err = parseFlags(base)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.otlpEndpoint).To(Equal(endpoint + ":4318"))
	})

	It("refuses one without the other, a bad endpoint and an invalid token, without printing the token", func() {
		tok := tokenFile("koinon_test_not-a-real-token-agentd-0001")
		_, err := parseFlags(otlp(endpoint, ""))
		Expect(err).To(MatchError(ContainSubstring("required together")))
		_, err = parseFlags(otlp("", tok))
		Expect(err).To(MatchError(ContainSubstring("required together")))
		_, err = parseFlags(otlp("koinon-api", tok))
		Expect(err).To(MatchError(ContainSubstring("http(s) URL")))
		_, err = parseFlags(otlp(endpoint+"/v1/traces", tok))
		Expect(err).To(MatchError(ContainSubstring("no path")))
		_, err = parseFlags(otlp(endpoint, filepath.Join(GinkgoT().TempDir(), "missing")))
		Expect(err).To(MatchError(ContainSubstring("--otlp-token-file")))
		_, err = parseFlags(otlp(endpoint, tokenFile("hunter2")))
		Expect(err).To(MatchError(ContainSubstring("at least")))
		Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
	})
})
