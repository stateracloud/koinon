// SPDX-License-Identifier: Apache-2.0

package codehost_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

func githubType() *connectors.Type {
	t, ok := connectors.Get(connectors.GitHubID)
	Expect(ok).To(BeTrue())
	return t
}

func certPEM() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "koinon test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

var _ = Describe("GitConfig", func() {
	It("scopes the credential helper and the CA to a connection's host", func() {
		loc, err := githubType().Locate("https://ghes.example.test")
		Expect(err).NotTo(HaveOccurred())
		cfg := string(codehost.GitConfig(loc, true))
		Expect(cfg).To(ContainSubstring("[credential \"https://ghes.example.test\"]\n"))
		Expect(cfg).To(ContainSubstring("\thelper = /usr/local/bin/koinon-git-credential --host ghes.example.test\n"))
		Expect(cfg).To(ContainSubstring("[http \"https://ghes.example.test/\"]\n\tsslCAInfo = /sandbox/.koinon/git/ca.pem\n"))
		Expect(cfg).NotTo(ContainSubstring("github.com"))
	})

	It("names the port when it is not 443, and leaves the CA out without a bundle", func() {
		loc, err := githubType().Locate("https://ghes.example.test:8443")
		Expect(err).NotTo(HaveOccurred())
		cfg := string(codehost.GitConfig(loc, false))
		Expect(cfg).To(ContainSubstring("[credential \"https://ghes.example.test:8443\"]"))
		Expect(cfg).To(ContainSubstring("--host ghes.example.test:8443\n"))
		Expect(cfg).NotTo(ContainSubstring("sslCAInfo"))
	})

	It("adds nothing but the CA on the default host, whose image config already has the helper", func() {
		loc := githubType().DefaultLocation()
		Expect(string(codehost.GitConfig(loc, false))).NotTo(ContainSubstring("["))
		cfg := string(codehost.GitConfig(loc, true))
		Expect(cfg).NotTo(ContainSubstring("[credential"))
		Expect(cfg).To(ContainSubstring("[http \"https://github.com/\"]"))
	})
})

var _ = Describe("CapabilitiesOf", func() {
	It("reads the descriptor from the connector data", func() {
		c, err := codehost.CapabilitiesOf(githubType())
		Expect(err).NotTo(HaveOccurred())
		Expect(c).To(Equal(codehost.Capabilities{
			DisplayName: "GitHub", CredentialClass: "app-installation", TokenLifetime: time.Hour,
			NarrowsRepositories: true, NarrowsPermissions: true, MaxRepositories: 500, CanDenyCI: true, ExactModes: true,
			CIPaths: []string{".github/workflows/"},
		}))
	})
})

var _ = Describe("Site", func() {
	It("reads the base URL and CA bundle from the connection's config", func() {
		s, err := codehost.SiteOf([]byte(`{"appId": 1, "baseUrl": "https://ghes.example.test", "caBundle": "x"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(s).To(Equal(codehost.Site{BaseURL: "https://ghes.example.test", CABundle: "x"}))
		s, err = codehost.SiteOf([]byte(`{"appId": 1}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(s).To(Equal(codehost.Site{}))
	})

	It("accepts certificates and nothing else as a CA bundle", func() {
		_, err := codehost.CertPool(certPEM())
		Expect(err).NotTo(HaveOccurred())
		_, err = codehost.CertPool("")
		Expect(err).To(HaveOccurred())
		_, err = codehost.CertPool("not pem")
		Expect(err).To(HaveOccurred())
		withKey := certPEM() + string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-a-real-key")}))
		_, err = codehost.CertPool(withKey)
		Expect(err).To(MatchError(ContainSubstring("only CERTIFICATE blocks")))
	})

	It("needs no pool without a bundle", func() {
		p, err := codehost.Site{}.Pool()
		Expect(err).NotTo(HaveOccurred())
		Expect(p).To(BeNil())
	})
})
