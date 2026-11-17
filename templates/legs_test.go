// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/templates"
)

// A template does not name MCP servers: an admin allows them on the agent. What a template does
// fix is its grants (the org's repositories, private data) and its tools, the structured spec.tools
// list. This test reads both, classifies each tool in spec.tools with sinkclass.Classify and
// sinkclass.Declaration.ResultLegs, the functions the guard calls, over the declarations in
// testdata/legs/tool-declarations.json (ADR 0084 decision 8, ADR 0091 decision 4), and fails when
// one template holds all three legs of the lethal trifecta (ADR 0084 decision 9). The legs come from
// spec.tools only, so a tool the instructions describe in prose ("the search and fetch tools") is
// read too. Parse fails when the instructions name a tool in backticks that spec.tools does
// not list.

// The org the classifier runs for: one code-host connection that lists one private repository, the
// repository a code-host tool of a template acts on (ADR 0091 decision 3).
const (
	testConnection = "conn-1"
	testOwner      = "acme"
	testRepo       = "app"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// declarations are the tool records an admin or a server's publisher sets (ADR 0091 decision 2),
// read from testdata/legs/tool-declarations.json in the shape the API takes them. A tool that is not
// in the file comes from an admin-allowed server nobody classified and gets sinkclass.Default() and
// the legs of mcpservers.DefaultDeclaration(): external-high with both data legs (decision 4).
func declarations() map[string]sinkclass.Declaration {
	declarationsOnce.Do(func() {
		var raw map[string]sinkclass.Declaration
		Expect(json.Unmarshal(readFile("testdata/legs/tool-declarations.json"), &raw)).To(Succeed())
		declarationsCache = map[string]sinkclass.Declaration{}
		for name, d := range raw {
			n, err := d.Normalize()
			Expect(err).NotTo(HaveOccurred(), name)
			declarationsCache[name] = n
		}
	})
	return declarationsCache
}

// The file is read and parsed once per process, not per call.
var (
	declarationsOnce  sync.Once
	declarationsCache map[string]sinkclass.Declaration
)

// testScope is the org's code-host scope for the classifier.
func testScope() sinkclass.Scope {
	return sinkclass.NewScope([]sinkclass.Repo{{
		Connection: testConnection, Owner: testOwner, Name: testRepo,
		Visibility: sinkclass.VisPrivate, CheckedAt: testNow,
	}}, 0)
}

// toolAddress is a call's address arguments for a tool: the org's repository for a code-host tool and
// a public URL for a fetch tool, the worst case the template's text allows for each.
func toolAddress(d sinkclass.Declaration) map[string]string {
	addr := map[string]string{}
	switch d.Derivation {
	case sinkclass.CodeHost:
		addr[d.AddressArgs[0]], addr[d.AddressArgs[1]] = testOwner, testRepo
	case sinkclass.Fetch:
		addr[d.AddressArgs[0]] = "https://example.com/page"
	}
	return addr
}

// classOf is the sink class of one call to the named tool, through sinkclass.Classify, the function
// the guard calls (internal/mcpguard/classify.go).
func classOf(name string) sinkclass.Class {
	d, ok := declarations()[name]
	if !ok {
		d = sinkclass.Default()
	}
	return sinkclass.Classify(d, toolAddress(d), testConnection, testScope(), nil, testNow)
}

// toolLegs returns the legs a named tool's results carry, through Declaration.ResultLegs, the
// function the guard calls (internal/mcpguard/result.go): its declared result legs, the server's
// legs where it declares none. A tool nobody declared has the default declaration.
func toolLegs(name string) []string {
	d, ok := declarations()[name]
	if !ok {
		d = sinkclass.Default()
	}
	server := mcpservers.DefaultDeclaration().Legs
	untrusted, private := d.ResultLegs(slices.Contains(server, mcpservers.LegUntrustedInput), slices.Contains(server, mcpservers.LegPrivateData))
	var legs []string
	if untrusted {
		legs = append(legs, mcpservers.LegUntrustedInput)
	}
	if private {
		legs = append(legs, mcpservers.LegPrivateData)
	}
	return legs
}

// toolsOf are the tool names of a template: its structured spec.tools list.
func toolsOf(t *templates.Template) []string {
	names := slices.Clone(t.Spec.Tools)
	slices.Sort(names)
	return slices.Compact(names)
}

// adminMCPTool is the declaration of a tool of an MCP server the admin allows for a template that
// names no tools of its own (finops-analyst): the cost and metrics tools of the template's servers.
// A template cannot name them. Declared is what the template asks the admin to set for a read-only
// query tool (testdata/legs/mcp-readonly-tool.json: sink class none, results private data). Not
// declared is what the guard uses for a tool nobody classified: sinkclass.Default() and the legs of
// mcpservers.DefaultDeclaration() (ADR 0091 decisions 2 and 4).
func adminMCPTool(declared bool) (sinkclass.Declaration, []string) {
	if !declared {
		return sinkclass.Default(), mcpservers.DefaultDeclaration().Legs
	}
	var d sinkclass.Declaration
	Expect(json.Unmarshal(readFile("testdata/legs/mcp-readonly-tool.json"), &d)).To(Succeed())
	d, err := d.Normalize()
	Expect(err).NotTo(HaveOccurred())
	return d, nil
}

// adminServedTools are the tools of spec.tools that an admin's MCP server serves, so Koinon ships no
// declaration for them: the declarations in testdata/legs/tool-declarations.json are what an admin
// sets by hand. Koinon's own tools (ask_user, the code-host tools) declare theirs in code.
var adminServedTools = map[string]bool{"web_url_read": true, "searxng_web_search": true}

type templateLegs struct {
	untrustedInput bool
	privateData    bool
	// autoOutbound is a tool whose call classifies external-high, the auto tier (ADR 0091 line 146).
	autoOutbound bool
}

func (l templateLegs) all() bool { return l.untrustedInput && l.privateData && l.autoOutbound }

// bothInputLegs is untrusted input and private data: the pair that makes the byte cap apply.
func (l templateLegs) bothInputLegs() bool { return l.untrustedInput && l.privateData }

// legsOf reads the legs a template gives an agent made from it, with the admin's MCP tools
// declared as the template asks.
func legsOf(t *templates.Template) templateLegs { return legsOfWith(t, true) }

// legsOfWith is legsOf where mcpDeclared says whether the admin classified the MCP tools of a
// template that points at admin servers.
func legsOfWith(t *templates.Template, mcpDeclared bool) templateLegs {
	var l templateLegs
	// Every grant is on a connector to a system the org owns: repositories and what they hold.
	if len(t.Spec.Grants) > 0 {
		l.privateData = true
	}
	// MCP servers the template points the agent at without naming them are internal data sources
	// (billing, cost, metrics); no web tool is among them.
	if strings.Contains(t.Spec.Instructions, "MCP") {
		l.privateData = true
		d, serverLegs := adminMCPTool(mcpDeclared)
		untrusted, private := d.ResultLegs(slices.Contains(serverLegs, mcpservers.LegUntrustedInput), slices.Contains(serverLegs, mcpservers.LegPrivateData))
		l.untrustedInput = l.untrustedInput || untrusted
		l.privateData = l.privateData || private
		// An admin tool has no address arguments to read: the call classifies as its declaration says.
		if sinkclass.Classify(d, nil, testConnection, testScope(), nil, testNow) == sinkclass.ExternalHigh {
			l.autoOutbound = true
		}
	}
	for _, name := range toolsOf(t) {
		if !mcpDeclared && adminServedTools[name] {
			// Nobody classified this tool: the guard uses the default and the server's legs.
			d, serverLegs := adminMCPTool(false)
			untrusted, private := d.ResultLegs(slices.Contains(serverLegs, mcpservers.LegUntrustedInput), slices.Contains(serverLegs, mcpservers.LegPrivateData))
			l.untrustedInput = l.untrustedInput || untrusted
			l.privateData = l.privateData || private
			l.autoOutbound = true
			continue
		}
		legs := toolLegs(name)
		if slices.Contains(legs, mcpservers.LegUntrustedInput) {
			l.untrustedInput = true
		}
		if slices.Contains(legs, mcpservers.LegPrivateData) {
			l.privateData = true
		}
		if classOf(name) == sinkclass.ExternalHigh {
			l.autoOutbound = true
		}
	}
	return l
}

var _ = Describe("Template legs (ADR 0084 decision 9)", func() {
	It("splits private research from web research", func() {
		r, ok := templates.Get("researcher")
		Expect(ok).To(BeTrue())
		Expect(legsOf(r)).To(Equal(templateLegs{privateData: true}), "researcher is private-only")
		Expect(r.Spec.Grants).NotTo(BeEmpty())

		w, ok := templates.Get("web-researcher")
		Expect(ok).To(BeTrue())
		Expect(legsOf(w)).To(Equal(templateLegs{untrustedInput: true}), "web-researcher has no private data")
		Expect(w.Spec.Tools).To(ContainElements("web_url_read", "searxng_web_search"))
		Expect(w.Spec.Grants).To(BeEmpty(), "web-researcher holds no grant")
		Expect(w.Spec.Instructions).To(ContainSubstring("web_url_read"))
		Expect(w.Spec.Instructions).NotTo(ContainSubstring("MCP"), "an MCP server of private data is not named")
	})

	It("ships no template that holds untrusted input, private data and an auto-tier outbound tool", func() {
		all, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range all {
			Expect(legsOf(t).all()).To(BeFalse(), t.Metadata.ID+" holds untrusted input, private data and an auto-tier outbound tool: split it")
		}
	})

	It("names the shipped templates that reach the approval branch with undeclared MCP tools (ADR 0091 decision 5)", func() {
		all, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		var reach []string
		for _, t := range all {
			if legsOfWith(t, false).all() {
				reach = append(reach, t.Metadata.ID)
			}
		}
		// These templates use tools of an admin's MCP servers. Those tools reach the approval branch
		// until the admin declares them (the template's description and the README say how).
		Expect(reach).To(Equal([]string{"finops-analyst", "lcm-upgrader", "web-researcher"}))
	})

	It("reports a template that holds all three legs (negative fixture)", func() {
		t, err := templates.Parse(readFile("testdata/legs/all-three.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(legsOf(t)).To(Equal(templateLegs{untrustedInput: true, privateData: true, autoOutbound: true}))
		Expect(legsOf(t).all()).To(BeTrue())
	})

	It("reports a template that names an unlisted tool (negative fixture)", func() {
		t, err := templates.Parse(readFile("testdata/legs/unlisted-tool.yaml"))
		Expect(err).NotTo(HaveOccurred())
		_, declared := declarations()["fetch_url"]
		Expect(declared).To(BeFalse(), "fetch_url is nobody's declared tool: the default applies")
		Expect(legsOf(t)).To(Equal(templateLegs{untrustedInput: true, privateData: true, autoOutbound: true}))
		Expect(legsOf(t).all()).To(BeTrue())
	})

	It("classifies a tool the instructions only describe in prose when spec.tools declares it", func() {
		t, err := templates.Parse(readFile("testdata/legs/prose-tool.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(templates.BacktickedTools(t.Spec.Instructions)).To(BeEmpty(), "no tool is backticked")
		Expect(t.Spec.Tools).To(Equal([]string{"web_url_read"}))
		Expect(legsOf(t)).To(Equal(templateLegs{untrustedInput: true}))
	})

	It("reads a declared unlisted tool as the default (fail closed)", func() {
		t, err := templates.Parse(readFile("testdata/legs/prose-tool.yaml"))
		Expect(err).NotTo(HaveOccurred())
		t.Spec.Tools = []string{"post_webhook"}
		Expect(legsOf(t)).To(Equal(templateLegs{untrustedInput: true, privateData: true, autoOutbound: true}))
	})

	It("classifies tools through sinkclass.Classify", func() {
		Expect(classOf("ask_user")).To(Equal(sinkclass.None))
		Expect(classOf("create_pull_request")).To(Equal(sinkclass.Internal), "the org's own repository")
		Expect(classOf("web_url_read")).To(Equal(sinkclass.ExternalLow))
		Expect(classOf("searxng_web_search")).To(Equal(sinkclass.ExternalLow))
		Expect(classOf("post_webhook")).To(Equal(sinkclass.ExternalHigh), "an unlisted tool is the default")
	})

	It("reports researcher again when it gets a web read tool back", func() {
		r, ok := templates.Get("researcher")
		Expect(ok).To(BeTrue())
		raw := strings.Replace(string(readFile("researcher.yaml")),
			"Read long files in parts, not whole.", "Read pages with `web_url_read`.", 1)
		Expect(raw).NotTo(Equal(string(readFile("researcher.yaml"))))
		_, err := templates.Parse([]byte(raw))
		Expect(err).To(MatchError(ContainSubstring("add it to spec.tools")), "Parse refuses the undeclared tool")
		raw = strings.Replace(raw, "  tools: [ask_user]\n", "  tools: [ask_user, web_url_read]\n", 1)
		changed, err := templates.Parse([]byte(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(changed.Metadata.ID).To(Equal(r.Metadata.ID))
		Expect(legsOf(changed).bothInputLegs()).To(BeTrue())
	})

	It("reports web-researcher again when it gets a repository grant back", func() {
		raw := strings.Replace(string(readFile("web-researcher.yaml")), "  grants: []\n",
			"  grants:\n    - connector: gitea\n      scopes: [repo.read]\n      preset: read-only\n      targets: {min: 1, max: 1}\n", 1)
		changed, err := templates.Parse([]byte(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(legsOf(changed).bothInputLegs()).To(BeTrue())
	})
})
