// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("The repo.read grant on a connection's own host", func() {
	cfg := agentconfig.Config{
		Grants:   []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
		Bindings: []agentconfig.Binding{{Connector: "github", ConnectionID: "00000000-0000-0000-0000-000000000001", Targets: []string{"acme/widgets"}}},
	}

	hosts := func(read reconciler.GitHubRead) []string {
		var out []string
		for _, e := range read.Endpoints {
			out = append(out, e.Host)
		}
		return out
	}

	It("renders github.com and api.github.com without a base URL", func() {
		read, ok, err := reconciler.GitHubReadAccessAt(cfg, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(hosts(read)).To(Equal([]string{"github.com", "api.github.com"}))
		Expect(read).To(Equal(mustRead(cfg)))
	})

	It("renders only the connection's host for a GitHub Enterprise Server", func() {
		read, ok, err := reconciler.GitHubReadAccessAt(cfg, "https://ghes.example.test")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(hosts(read)).To(Equal([]string{"ghes.example.test"}))
		e := read.Endpoints[0]
		Expect(e.Port).To(BeEquivalentTo(443))
		Expect(e.Protocol).To(Equal("rest"))
		Expect(e.Allow).To(ContainElements(
			execution.L7Rule{Method: "POST", Path: "/acme/widgets.git/git-upload-pack"},
			execution.L7Rule{Method: "GET", Path: "/api/v3/repos/acme/widgets"},
		))
		Expect(e.Deny).To(ContainElement(execution.L7Rule{Method: "POST", Path: "/api/v3/**"}))

		// The provider profile and the policy entry carry the same endpoints, so the token is
		// only ever sent to the connection's host.
		profile := reconciler.GitHubProfile("p", read, reconciler.DefaultGitBinaries)
		Expect(profile.Endpoints).To(Equal(read.Endpoints))
		Expect(reconciler.GitHubRule(read, reconciler.DefaultGitBinaries).Endpoints).To(Equal(read.Endpoints))
	})

	It("keeps the port of the base URL", func() {
		read, _, err := reconciler.GitHubReadAccessAt(cfg, "https://ghes.example.test:8443")
		Expect(err).NotTo(HaveOccurred())
		Expect(read.Endpoints[0].Port).To(BeEquivalentTo(8443))
	})

	It("refuses a base URL that is not a bare https host", func() {
		_, _, err := reconciler.GitHubReadAccessAt(cfg, "http://ghes.example.test")
		Expect(err).To(HaveOccurred())
	})
})

func mustRead(cfg agentconfig.Config) reconciler.GitHubRead {
	read, ok, err := reconciler.GitHubReadAccess(cfg)
	Expect(err).NotTo(HaveOccurred())
	Expect(ok).To(BeTrue())
	return read
}
