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
		Expect(t.MCPHosts(locate(t, ""), nil)).To(Equal([]string{"github.com", "api.github.com", "api.githubcopilot.com"}))
	})

	It("are, for a connection on its own host, only that host", func() {
		Expect(githubType().MCPHosts(locate(githubType(), "https://ghe.example.com:8443"), nil)).To(Equal([]string{"ghe.example.com"}))
		Expect(giteaType().MCPHosts(locate(giteaType(), "https://git.example.com/forge"), nil)).To(Equal([]string{"git.example.com"}))
	})

	It("add the admin's extra hosts of the connection, on every location, without duplicates (#240)", func() {
		extra := []string{"gitea-mcp.example.com", "git.example.com", "Gitea-MCP.example.com."}
		Expect(giteaType().MCPHosts(locate(giteaType(), "https://git.example.com/forge"), extra)).To(Equal([]string{"git.example.com", "gitea-mcp.example.com"}))
		Expect(githubType().MCPHosts(locate(githubType(), ""), []string{"mcp.corp.example"})).To(ContainElement("mcp.corp.example"))
	})

	It("link a server on an extra host, and still refuse every other host (#240)", func() {
		t, loc := giteaType(), locate(giteaType(), "https://git.example.com")
		extra := []string{"gitea-mcp"}
		Expect(t.ServesMCP(loc, nil, "https://gitea-mcp/mcp")).To(MatchError(ContainSubstring("accepts MCP servers at git.example.com)")))
		Expect(t.ServesMCP(loc, extra, "https://gitea-mcp:8443/mcp")).To(Succeed())
		Expect(t.ServesMCP(loc, extra, "http://gitea-mcp/mcp")).To(MatchError(ContainSubstring("must use https")))
		Expect(t.ServesMCP(loc, extra, "https://other-mcp/mcp")).To(MatchError(ContainSubstring("git.example.com, gitea-mcp)")))
		Expect(t.ServesMCP(loc, extra, "https://x.gitea-mcp/mcp")).NotTo(Succeed())
	})

	DescribeTable("ParseMCPHost",
		func(in, want, refusal string) {
			got, err := connectors.ParseMCPHost(in)
			if refusal != "" {
				Expect(err).To(MatchError(ContainSubstring(refusal)))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("a plain name", "gitea-mcp.example.com", "gitea-mcp.example.com", ""),
		Entry("a single label", "gitea-mcp", "gitea-mcp", ""),
		Entry("upper case with a trailing dot", " Gitea-MCP.Example.com. ", "gitea-mcp.example.com", ""),
		Entry("empty", "", "", "empty"),
		Entry("a port", "mcp.example.com:8443", "", "not a plain host name"),
		Entry("a scheme", "https://mcp.example.com", "", "not a plain host name"),
		Entry("a path", "mcp.example.com/mcp", "", "not a plain host name"),
		Entry("a wildcard", "*.example.com", "", "not a plain host name"),
		Entry("user info", "a@mcp.example.com", "", "not a plain host name"),
		Entry("an IPv4 address", "10.0.0.5", "", "IP address"),
		Entry("an IPv6 address", "::1", "", "IP address"),
		Entry("an empty label", "a..example.com", "", "not a host name"),
		Entry("a label with a leading hyphen", "-a.example.com", "", "not a host name"),
		Entry("an underscore", "a_b.example.com", "", "not a plain host name"),
		Entry("a label over 63 characters", strings.Repeat("a", 64)+".example.com", "", "not a host name"),
		Entry("a short IPv4 form", "127.1", "", "IP address"),
		Entry("a dotted hex IPv4", "0x7f.1", "", "reads as an IP address"),
		Entry("a hex last label", "mcp.0x7f000001", "", "reads as an IP address"),
		Entry("a decimal IPv4", "2130706433", "", "IP address"),
		Entry("a name ending in a number", "mcp.example.123", "", "reads as an IP address"),
		Entry("a non-ASCII name", "mcp.exämple.com", "", "not a plain host name"),
		Entry("a punycode label", "xn--exmple-cua.example.com", "", "xn--"),
		Entry("a label that only starts with digits", "1mcp.example.com", "1mcp.example.com", ""),
	)

	DescribeTable("ReservedMCPHost",
		func(host string, reserved bool) {
			_, ok := connectors.ReservedMCPHost(host)
			Expect(ok).To(Equal(reserved))
		},
		Entry("the GitHub web host", "github.com", true),
		Entry("the GitHub API host", "API.github.com.", true),
		Entry("GitHub's MCP host", "api.githubcopilot.com", true),
		Entry("a company host", "gitea-mcp.corp.example", false),
	)

	It("names the egress list when it refuses a private-looking host", func() {
		t, loc := giteaType(), locate(giteaType(), "https://git.example.com")
		Expect(t.ServesMCP(loc, nil, "https://gitea-mcp.corp.internal/mcp")).To(MatchError(ContainSubstring("egress.allowedInternalHosts")))
		Expect(t.ServesMCP(loc, nil, "https://mcp.example.org/mcp")).NotTo(MatchError(ContainSubstring("egress.allowedInternalHosts")))
	})

	DescribeTable("ServesMCP",
		func(t func() *connectors.Type, baseURL, serverURL, refusal string) {
			err := t().ServesMCP(locate(t(), baseURL), nil, serverURL)
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
