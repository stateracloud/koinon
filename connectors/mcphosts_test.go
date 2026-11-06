// SPDX-License-Identifier: Apache-2.0

package connectors_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
)

var _ = Describe("the MCP hosts that act on a connection's host (#117)", func() {
	locate := func(t *connectors.Type, baseURL string) connectors.Location {
		GinkgoHelper()
		loc, err := t.Locate(baseURL)
		Expect(err).NotTo(HaveOccurred())
		return loc
	}

	It("are, for github.com, its web and API hosts and GitHub's MCP server", func() {
		t := githubType()
		Expect(t.MCPHosts(locate(t, ""))).To(Equal([]string{"github.com", "api.github.com", "api.githubcopilot.com"}))
	})

	It("are, for a connection on its own host, only that host", func() {
		Expect(githubType().MCPHosts(locate(githubType(), "https://ghe.example.com:8443"))).To(Equal([]string{"ghe.example.com"}))
		Expect(giteaType().MCPHosts(locate(giteaType(), "https://git.example.com/forge"))).To(Equal([]string{"git.example.com"}))
	})

	DescribeTable("ServesMCP",
		func(t func() *connectors.Type, baseURL, serverURL, refusal string) {
			err := t().ServesMCP(locate(t(), baseURL), serverURL)
			if refusal == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(ContainSubstring(refusal)))
		},
		Entry("GitHub's MCP server on github.com", githubType, "", "https://api.githubcopilot.com/mcp/", ""),
		Entry("the github.com API host", githubType, "", "https://api.github.com/mcp", ""),
		Entry("a host name in another case, with a trailing dot", githubType, "", "https://API.githubcopilot.com./mcp/", ""),
		Entry("a GitHub Enterprise Server's own host, on any port", githubType, "https://ghe.example.com", "https://ghe.example.com:9000/mcp", ""),
		Entry("a Gitea's own host", giteaType, "https://git.example.com/forge", "https://git.example.com/mcp", ""),
		Entry("a Gitea's own host over http", giteaType, "https://git.example.com/forge", "http://git.example.com/mcp", "must use https"),
		Entry("GitHub's MCP server over http", githubType, "", "http://api.githubcopilot.com/mcp/", "must use https"),
		Entry("GitHub's MCP server on a Gitea", giteaType, "https://git.example.com", "https://api.githubcopilot.com/mcp/",
			"an MCP server at api.githubcopilot.com does not act on the Gitea connection at https://git.example.com; link it to the connection of its own code host (this connection accepts MCP servers at git.example.com)"),
		Entry("GitHub's MCP server on a GitHub Enterprise Server", githubType, "https://ghe.example.com", "https://api.githubcopilot.com/mcp/", "accepts MCP servers at ghe.example.com)"),
		Entry("a Gitea's host on github.com", githubType, "", "https://git.example.com/mcp", "does not act on the GitHub connection at https://github.com"),
		Entry("a sub-domain of the connection's host", giteaType, "https://git.example.com", "https://mcp.git.example.com/mcp", "mcp.git.example.com"),
		Entry("a host that ends like the connection's", githubType, "", "https://evilgithub.com/mcp", "evilgithub.com"),
		Entry("a URL without a host", githubType, "", "mcp", "has no host"),
	)

	DescribeTable("Parse rejects mcpHosts",
		func(entry, want string) {
			doc := githubYAML()
			from := "      - api.githubcopilot.com\n"
			Expect(doc).To(ContainSubstring(from))
			_, err := connectors.Parse([]byte(strings.Replace(doc, from, entry, 1)))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("with a port", "      - api.githubcopilot.com:443\n", "schema"),
		Entry("with a scheme", "      - https://api.githubcopilot.com\n", "schema"),
		Entry("in upper case", "      - API.githubcopilot.com\n", "schema"),
		Entry("as a wildcard", "      - '*.githubcopilot.com'\n", "schema"),
		Entry("without a dot", "      - localhost\n", "schema"),
		Entry("on a selfHosted connector", "      - api.githubcopilot.com\n    selfHosted: true\n", "cannot have mcpHosts"),
	)
})
