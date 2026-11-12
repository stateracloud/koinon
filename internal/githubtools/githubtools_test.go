// SPDX-License-Identifier: Apache-2.0

package githubtools_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/test/scenarios"
)

func file(p, content string) githubtools.FileChange {
	return githubtools.FileChange{Path: p, Content: content}
}

// githubGuard is the workflow guard of a GitHub connection with no configured directories.
func githubGuard() codehost.Guard {
	g, err := codehost.NewGuard(codehost.Capabilities{CIPaths: []string{".github/workflows/"}, CIPathsFoldCase: true}, nil)
	Expect(err).NotTo(HaveOccurred())
	return g
}

func validPR() githubtools.PullRequestArgs {
	return githubtools.PullRequestArgs{
		Branch: "koinon/fix-typo", Title: "Fix a typo in the README", Body: "Fixes the typo.",
		Files: []githubtools.FileChange{file("README.md", "# Widgets\n")},
	}
}

func manyFiles(n int) []githubtools.FileChange {
	out := make([]githubtools.FileChange, n)
	for i := range out {
		out[i] = file(strings.Repeat("d/", 1)+"f"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".txt", "x")
	}
	return out
}

var _ = DescribeTable("ValidatePullRequest",
	func(mutate func(*githubtools.PullRequestArgs), want string) {
		a := validPR()
		mutate(&a)
		err := githubtools.ValidatePullRequest(a, githubGuard())
		if want == "" {
			Expect(err).NotTo(HaveOccurred())
			return
		}
		Expect(err).To(MatchError(ContainSubstring(want)))
	},
	Entry("accepts a small change", func(*githubtools.PullRequestArgs) {}, ""),
	Entry("accepts a deletion", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, githubtools.FileChange{Path: "old.txt", Delete: true})
	}, ""),
	Entry("accepts exactly 100 files", func(a *githubtools.PullRequestArgs) { a.Files = manyFiles(100) }, ""),
	Entry("accepts exactly 1 MiB", func(a *githubtools.PullRequestArgs) {
		a.Files = []githubtools.FileChange{file("a.txt", strings.Repeat("a", 1<<19)), file("b.txt", strings.Repeat("b", 1<<19))}
	}, ""),
	Entry("refuses a workflow", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, file(".github/workflows/ci.yml", "on: push"))
	}, "under .github/workflows/"),
	Entry("refuses a workflow in another case", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, file(".GitHub/Workflows/ci.yml", "on: push"))
	}, "under .github/workflows/"),
	Entry("refuses deleting a workflow", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, githubtools.FileChange{Path: ".github/workflows/ci.yml", Delete: true})
	}, "under .github/workflows/"),
	Entry("refuses 101 files", func(a *githubtools.PullRequestArgs) { a.Files = manyFiles(101) }, "101 files is more than the limit of 100"),
	Entry("refuses more than 1 MiB", func(a *githubtools.PullRequestArgs) {
		a.Files = []githubtools.FileChange{file("a.txt", strings.Repeat("a", 1<<20)), file("b.txt", "b")}
	}, "more than the limit of 1048576"),
	Entry("refuses ../ traversal", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "../etc/passwd" }, "path traversal"),
	Entry("refuses traversal in the middle", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "docs/../../x" }, "path traversal"),
	Entry("refuses an absolute path", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "/etc/passwd" }, "absolute"),
	Entry("refuses a backslash", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = `docs\..\x` }, "backslash"),
	Entry("refuses an unclean path", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "docs//x" }, "not clean"),
	Entry("refuses ./", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "./x" }, "not clean"),
	Entry("refuses .git", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = ".git/hooks/pre-commit" }, "inside .git"),
	Entry("refuses a NUL in a path", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "a\x00b" }, "control characters"),
	Entry("refuses an empty path", func(a *githubtools.PullRequestArgs) { a.Files[0].Path = "" }, "empty path"),
	Entry("refuses a path listed twice", func(a *githubtools.PullRequestArgs) { a.Files = append(a.Files, a.Files[0]) }, "listed twice"),
	Entry("refuses a deletion with content", func(a *githubtools.PullRequestArgs) { a.Files[0].Delete = true }, "deleted but has content"),
	Entry("accepts an executable file", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, githubtools.FileChange{Path: "scripts/check.sh", Content: "#!/bin/sh\n", Executable: true})
	}, ""),
	Entry("refuses an executable deletion", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, githubtools.FileChange{Path: "old.sh", Delete: true, Executable: true})
	}, "deleted but marked executable"),
	Entry("refuses an executable workflow", func(a *githubtools.PullRequestArgs) {
		a.Files = append(a.Files, githubtools.FileChange{Path: ".github/workflows/ci.yml", Content: "on: push", Executable: true})
	}, "under .github/workflows/"),
	Entry("refuses no files", func(a *githubtools.PullRequestArgs) { a.Files = nil }, "files is empty"),
	Entry("refuses a branch outside koinon/", func(a *githubtools.PullRequestArgs) { a.Branch = "main" }, "must be a new branch name starting with koinon/"),
	Entry("refuses the bare prefix", func(a *githubtools.PullRequestArgs) { a.Branch = "koinon/" }, "starting with koinon/"),
	Entry("refuses a bad ref", func(a *githubtools.PullRequestArgs) { a.Branch = "koinon/a..b" }, "starting with koinon/"),
	Entry("refuses a .lock ref", func(a *githubtools.PullRequestArgs) { a.Branch = "koinon/x.lock" }, "starting with koinon/"),
	Entry("refuses a bad base", func(a *githubtools.PullRequestArgs) { a.Base = "-main" }, "base"),
	Entry("refuses a multi-line title", func(a *githubtools.PullRequestArgs) { a.Title = "a\nb" }, "title must be one line"),
	Entry("refuses an empty title", func(a *githubtools.PullRequestArgs) { a.Title = "" }, "title must be one line"),
	Entry("refuses a huge body", func(a *githubtools.PullRequestArgs) { a.Body = strings.Repeat("b", 65537) }, "body"),
)

var _ = Describe("ValidatePullRequest", func() {
	It("reports every problem at once", func() {
		a := validPR()
		a.Branch = "main"
		a.Files = append(a.Files, file(".github/workflows/x.yml", ""), file("../x", ""))
		var ve *githubtools.ValidationError
		Expect(errors.As(githubtools.ValidatePullRequest(a, githubGuard()), &ve)).To(BeTrue())
		Expect(ve.Reasons).To(HaveLen(3))
	})
})

var _ = DescribeTable("ValidateComment",
	func(a githubtools.CommentArgs, want string) {
		err := githubtools.ValidateComment(a)
		if want == "" {
			Expect(err).NotTo(HaveOccurred())
			return
		}
		Expect(err).To(MatchError(ContainSubstring(want)))
	},
	Entry("accepts a comment", githubtools.CommentArgs{IssueNumber: 7, Body: "Thanks!"}, ""),
	Entry("refuses issue 0", githubtools.CommentArgs{IssueNumber: 0, Body: "x"}, "issue_number"),
	Entry("refuses an empty body", githubtools.CommentArgs{IssueNumber: 1, Body: "  "}, "body"),
)

var _ = Describe("AssessRisk", func() {
	It("flags push-and-run risk", func() {
		r := githubtools.AssessRisk(githubtools.PullRequestArgs{Files: []githubtools.FileChange{
			file("README.md", "hi"),
			file(".github/dependabot.yml", "version: 2"),
			file("scripts/release.sh", "echo"),
			file("tools/run", "#!/bin/sh\necho"),
			file("Makefile", "all:"),
			file("go.mod", "module x"),
			file("web/package.json", "{}"),
			file("logo.png", "x"),
			file("data.bin.txt", "a\x00b"),
			{Path: "old.txt", Delete: true},
			{Path: "bin/tool", Content: "x", Executable: true},
		}})
		Expect(r.Files).To(Equal(11))
		Expect(r.CI).To(Equal([]string{".github/dependabot.yml"}))
		Expect(r.Scripts).To(Equal([]string{"scripts/release.sh", "tools/run", "Makefile"}))
		Expect(r.Dependencies).To(Equal([]string{"go.mod", "web/package.json"}))
		Expect(r.Binary).To(Equal([]string{"logo.png", "data.bin.txt"}))
		Expect(r.Deletions).To(Equal([]string{"old.txt"}))
		Expect(r.Executable).To(Equal([]string{"bin/tool"}))
	})
})

var _ = Describe("Canonical", func() {
	It("sorts keys and keeps text unescaped", func() {
		b, err := githubtools.Canonical(map[string]any{"b": 1, "a": "<x> & é", "c": map[string]any{"z": true, "y": nil}})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal(`{"a":"<x> & é","b":1,"c":{"y":null,"z":true}}`))
	})
})

// agentHeader sets the agent header the way agentgateway does.
type agentHeader struct {
	agent string
	base  http.RoundTripper
}

func (a agentHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if a.agent != "" {
		r.Header.Set(gateway.AgentHeader, a.agent)
	}
	return a.base.RoundTrip(r)
}

const standardGrant = `{"instructions":"help",` +
	`"grants":[{"connector":"github","scopes":["repo.read","pull_request.create","issue.comment"],"preset":"standard"}],` +
	`"bindings":[{"connector":"github","connectionId":"00000000-0000-0000-0000-000000000000","targets":["acme/widgets"]}]}`

var _ = Describe("Server", func() {
	var (
		srv  *httptest.Server
		logs *bytes.Buffer
	)

	BeforeEach(func() {
		needDB()
		logs = &bytes.Buffer{}
		s, err := githubtools.New(pool, githubtools.Options{Logger: slog.New(slog.NewJSONHandler(logs, nil))})
		Expect(err).NotTo(HaveOccurred())
		srv = httptest.NewServer(s.Handler())
		DeferCleanup(srv.Close)
	})

	session := func(ctx context.Context, agent string) *mcp.ClientSession {
		GinkgoHelper()
		client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0.0.1"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             srv.URL,
			HTTPClient:           &http.Client{Transport: agentHeader{agent: agent, base: http.DefaultTransport}},
			DisableStandaloneSSE: true,
			MaxRetries:           -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = s.Close() })
		return s
	}

	call := func(ctx context.Context, s *mcp.ClientSession, tool string, args any) (*mcp.CallToolResult, string) {
		GinkgoHelper()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		Expect(err).NotTo(HaveOccurred())
		var text []string
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				text = append(text, t.Text)
			}
		}
		return res, strings.Join(text, "\n")
	}

	result := func(res *mcp.CallToolResult) githubtools.Result {
		GinkgoHelper()
		b, err := json.Marshal(res.StructuredContent)
		Expect(err).NotTo(HaveOccurred())
		var out githubtools.Result
		Expect(json.Unmarshal(b, &out)).To(Succeed())
		return out
	}

	It("lists the three tools", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		res, err := session(ctx, f.agentID).ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, t := range res.Tools {
			names = append(names, t.Name)
		}
		Expect(names).To(ConsistOf("create_pull_request", "comment_on_issue", "get_approval_status"))
	})

	It("advertises the optional executable flag on create_pull_request's files", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		res, err := session(ctx, f.agentID).ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		var schema struct {
			Properties struct {
				Files struct {
					Items struct {
						Properties map[string]struct {
							Type        string `json:"type"`
							Description string `json:"description"`
						} `json:"properties"`
						Required []string `json:"required"`
					} `json:"items"`
				} `json:"files"`
			} `json:"properties"`
		}
		for _, t := range res.Tools {
			if t.Name != "create_pull_request" {
				continue
			}
			b, err := json.Marshal(t.InputSchema)
			Expect(err).NotTo(HaveOccurred())
			Expect(json.Unmarshal(b, &schema)).To(Succeed())
		}
		items := schema.Properties.Files.Items
		Expect(items.Properties).To(HaveKey("executable"))
		Expect(items.Properties["executable"].Type).To(Equal("boolean"))
		Expect(items.Properties["executable"].Description).To(ContainSubstring("100755"))
		Expect(items.Required).NotTo(ContainElement("executable"))
	})

	It("publishes the readiness prompt", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		res, err := session(ctx, f.agentID).ListPrompts(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Prompts).To(ConsistOf(HaveField("Name", gateway.ReadyPrompt)))
		got, err := session(ctx, f.agentID).GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyPrompt})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Messages).To(HaveLen(1))
	})

	// The kind e2e's repo-helper scenario (test/scenarios): the call the fake model makes is
	// filed under the agent's namespace, which is the branch the e2e expects on the fake GitHub
	// (old issue 281 changed the naming and only the e2e noticed).
	It("files the repo-helper scenario's pull request under the agent's namespace", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		p := scenarios.PullRequest
		args, err := json.Marshal(map[string]any{
			"branch": p.Suggestion, "title": p.Title, "body": "Adds hello.md as requested in the task.",
			"files": []map[string]string{{"path": p.File, "content": p.Content}},
		})
		Expect(err).NotTo(HaveOccurred())
		res, text := call(ctx, session(ctx, f.agentID), p.ParkingTool, json.RawMessage(args))
		Expect(res.IsError).To(BeFalse(), text)

		// Spelled out by hand, not through the code under test: koinon/<slug of the display name>-<last
		// eight hex digits of the agent ID>/<suggestion without its koinon/ prefix>. A change to the
		// naming rule must fail here and be made in test/e2e's expectation deliberately.
		hexID := strings.ReplaceAll(f.agent.ID.String(), "-", "")
		want := "koinon/helper-" + hexID[len(hexID)-8:] + "/add-hello-md"
		Expect(p.Suggestion).To(Equal("koinon/add-hello-md"))
		got, err := p.Branch("helper", f.agent.ID.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want), "the e2e expects the same branch")
		rows, err := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: f.org.ID, TaskID: f.run.TaskID})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		var recorded githubtools.PullRequestArgs
		Expect(json.Unmarshal([]byte(rows[0].ArgsJson), &recorded)).To(Succeed())
		Expect(recorded.Branch).To(Equal(want), "the approval shows the branch the e2e expects")
		Expect(recorded.Title).To(Equal(p.Title))
		Expect(recorded.Files).To(ConsistOf(file(p.File, p.Content)))
		Expect(rows[0].Tool).To(Equal(p.ParkingTool))
		Expect(rows[0].Status).To(Equal(store.ApprovalStatusPending), "the run parks on the approval")
	})

	It("records a pending approval with a stable hash and returns at once", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		s := session(ctx, f.agentID)

		// The same call twice, with keys and files in another order and different
		// whitespace: the hash covers the canonical form, so it is one request.
		raw1 := json.RawMessage(`{"branch":"koinon/fix","title":"Fix","files":[{"path":"b.txt","content":"B"},{"path":"a.txt","content":"A"}]}`)
		raw2 := json.RawMessage(`{ "files": [ {"content":"A","path":"a.txt"}, {"path":"b.txt", "content":"B"} ],
			"title":"Fix", "branch":"koinon/fix" }`)
		res1, text := call(ctx, s, "create_pull_request", raw1)
		Expect(res1.IsError).To(BeFalse(), text)
		Expect(text).To(MatchRegexp(`^approval requested: id=[0-9a-f-]{36} status=pending args_sha256=[0-9a-f]{64}\.`))
		r1 := result(res1)
		res2, _ := call(ctx, s, "create_pull_request", raw2)
		r2 := result(res2)
		Expect(r2.ID).To(Equal(r1.ID))
		Expect(r2.ArgsSHA256).To(Equal(r1.ArgsSHA256))

		By("the record holds the exact canonical call")
		rows, err := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: f.org.ID, TaskID: f.run.TaskID})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		row := rows[0]
		Expect(row.ArgsJson).To(Equal(`{"branch":"` + githubtools.BranchPrefix + githubtools.AgentNamespace("helper", f.agent.ID.String()) + `/fix","files":[{"content":"A","path":"a.txt"},{"content":"B","path":"b.txt"}],"title":"Fix"}`))
		Expect(row.ArgsSha256).To(Equal(githubtools.HashArgs([]byte(row.ArgsJson))))
		Expect(row.ArgsSha256).To(Equal(r1.ArgsSHA256))
		Expect(row.Tool).To(Equal("create_pull_request"))
		Expect(row.Target).To(Equal("github:acme/widgets"))
		Expect(row.AgentID).To(Equal(f.agent.ID))
		Expect(row.RunID).To(Equal(f.run.ID))
		Expect(row.TaskID).To(Equal(f.run.TaskID))
		Expect(row.Status).To(Equal(store.ApprovalStatusPending))
		Expect(row.Decision).To(Equal(store.ApprovalDecisionAsk))
		Expect(row.ExpiresAt.Time).To(BeTemporally("~", row.CreatedAt.Time.Add(githubtools.DefaultApprovalTTL), 5e9))
		var risk githubtools.Risk
		Expect(json.Unmarshal(row.Risk, &risk)).To(Succeed())
		Expect(risk.Files).To(Equal(2))

		By("different arguments are a different request")
		res3, _ := call(ctx, s, "create_pull_request", json.RawMessage(`{"branch":"koinon/fix","title":"Fix","base":"release","files":[{"path":"a.txt","content":"A"},{"path":"b.txt","content":"B"}]}`))
		Expect(result(res3).ID).NotTo(Equal(r1.ID))
		Expect(result(res3).ArgsSHA256).NotTo(Equal(r1.ArgsSHA256))

		By("arguments are not logged")
		Expect(logs.String()).To(ContainSubstring(`"msg":"approval requested"`))
		Expect(logs.String()).NotTo(ContainSubstring("a.txt"))
	})

	It("pins the hash of a known call", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		res, text := call(ctx, session(ctx, f.agentID), "comment_on_issue", map[string]any{"issue_number": 7, "body": "Thanks <3"})
		Expect(res.IsError).To(BeFalse(), text)
		// Computed independently of the package: SHA-256 of the canonical bytes.
		sum := sha256.Sum256([]byte(`{"body":"Thanks <3","issue_number":7}`))
		Expect(result(res).ArgsSHA256).To(Equal(hex.EncodeToString(sum[:])))
	})

	It("refuses invalid calls with the reason, recording nothing", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		s := session(ctx, f.agentID)
		res, text := call(ctx, s, "create_pull_request", map[string]any{
			"branch": "koinon/ci", "title": "CI",
			"files": []map[string]any{{"path": ".github/workflows/ci.yml", "content": "on: push"}},
		})
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("under .github/workflows/"))

		res, text = call(ctx, s, "create_pull_request", map[string]any{
			"branch": "koinon/x", "title": "X", "files": []map[string]any{{"path": "../x", "content": "x"}},
		})
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("path traversal"))

		res, _ = call(ctx, s, "create_pull_request", map[string]any{"branch": "koinon/x", "title": "X", "files": []any{}, "extra": 1})
		Expect(res.IsError).To(BeTrue())

		rows, err := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: f.org.ID, TaskID: f.run.TaskID})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
	})

	It("answers get_approval_status for the agent's own requests only", func(ctx SpecContext) {
		f := newFixture(ctx, standardGrant)
		s := session(ctx, f.agentID)
		res, _ := call(ctx, s, "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
		id := result(res).ID

		res, text := call(ctx, s, "get_approval_status", map[string]any{"id": id})
		Expect(res.IsError).To(BeFalse(), text)
		Expect(result(res).Status).To(Equal("pending"))
		Expect(result(res).Tool).To(Equal("comment_on_issue"))

		other := newFixture(ctx, standardGrant)
		res, text = call(ctx, session(ctx, other.agentID), "get_approval_status", map[string]any{"id": id})
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("no approval request"))
	})

	It("refuses calls without the gateway's agent header", func(ctx SpecContext) {
		res, text := call(ctx, session(ctx, ""), "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("routed through Koinon's agentgateway"))
		res, _ = call(ctx, session(ctx, "nope/00000000-0000-0000-0000-000000000000"), "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
		Expect(res.IsError).To(BeTrue())
	})

	It("refuses tools the grant does not allow", func(ctx SpecContext) {
		f := newFixture(ctx, `{"instructions":"help","grants":[{"connector":"github","scopes":["repo.read","pull_request.create"],"preset":"read-only"}],`+
			`"bindings":[{"connector":"github","connectionId":"x","targets":["acme/widgets"]}]}`)
		s := session(ctx, f.agentID)
		res, text := call(ctx, s, "create_pull_request", validPRMap())
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("does not allow create_pull_request"))
		res, text = call(ctx, s, "comment_on_issue", map[string]any{"issue_number": 3, "body": "x"})
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("does not allow comment_on_issue"))
	})

	It("refuses a grant without a repository", func(ctx SpecContext) {
		f := newFixture(ctx, `{"instructions":"help","grants":[{"connector":"github","scopes":["pull_request.create"],"preset":"standard"}]}`)
		res, text := call(ctx, session(ctx, f.agentID), "create_pull_request", validPRMap())
		Expect(res.IsError).To(BeTrue())
		Expect(text).To(ContainSubstring("no repository"))
	})

	It("parks a call the Trusted preset allows for a person when no leak check is configured", func(ctx SpecContext) {
		f := newFixture(ctx, trustedGrant)
		res, text := call(ctx, session(ctx, f.agentID), "create_pull_request", validPRMap())
		Expect(res.IsError).To(BeFalse(), text)
		Expect(result(res).Status).To(Equal("pending"))
		Expect(decisionOf(ctx, result(res).ID)).To(Equal(store.ApprovalDecisionAsk))
	})

	// The guard lets the agent's calls to these tools through (mcpguard.Options.OwnTargets) because a
	// person approves the exact call. A call the Trusted preset allows or an approval scope covers
	// has no such person, so it runs only once the leak check finds it clean (#687).
	Describe("the leak check of a call no person decides", func() {
		var (
			checked []githubtools.LeakCall
			answer  map[string]any
			failure error
		)

		BeforeEach(func() {
			checked, answer, failure = nil, nil, nil
			s, err := githubtools.New(pool, githubtools.Options{
				Logger: slog.New(slog.NewJSONHandler(logs, nil)),
				LeakCheck: func(_ context.Context, c githubtools.LeakCall) (map[string]any, error) {
					checked = append(checked, c)
					return answer, failure
				},
			})
			Expect(err).NotTo(HaveOccurred())
			srv = httptest.NewServer(s.Handler())
			DeferCleanup(srv.Close)
		})

		leakOf := func(ctx context.Context, id string) map[string]any {
			GinkgoHelper()
			var raw []byte
			Expect(pool.QueryRow(ctx, `SELECT risk FROM approval_requests WHERE id = $1`, id).Scan(&raw)).To(Succeed())
			var risk map[string]any
			Expect(json.Unmarshal(raw, &risk)).To(Succeed())
			leak, _ := risk["leak"].(map[string]any)
			return leak
		}

		It("runs a Trusted call the check finds clean, and hands the check the call", func(ctx SpecContext) {
			f := newFixture(ctx, trustedGrant)
			res, text := call(ctx, session(ctx, f.agentID), "create_pull_request", validPRMap())
			Expect(res.IsError).To(BeFalse(), text)
			Expect(text).To(HavePrefix("accepted: id="))
			Expect(result(res).Status).To(Equal("approved"))
			Expect(checked).To(HaveLen(1))
			c := checked[0]
			Expect(c.Org.ID).To(Equal(f.org.ID))
			Expect(c.Agent.ID).To(Equal(f.agent.ID))
			Expect(c.Tool).To(Equal(githubtools.ToolCreatePullRequest))
			Expect(c.Repo).To(Equal("acme/widgets"))
			Expect(c.Connection.Valid).To(BeTrue())
			Expect(c.Connection.String()).To(Equal("00000000-0000-0000-0000-000000000000"))
			var args map[string]any
			Expect(json.Unmarshal(c.Arguments, &args)).To(Succeed())
			Expect(args).To(HaveKeyWithValue("title", "Fix a typo"))
		})

		It("parks a Trusted call the check holds for a person, with the check's risk on the card", func(ctx SpecContext) {
			answer = map[string]any{"leak": map[string]any{"reasons": []string{"taint"}}}
			f := newFixture(ctx, trustedGrant)
			res, text := call(ctx, session(ctx, f.agentID), "create_pull_request", validPRMap())
			Expect(res.IsError).To(BeFalse(), text)
			Expect(result(res).Status).To(Equal("pending"))
			Expect(decisionOf(ctx, result(res).ID)).To(Equal(store.ApprovalDecisionAsk))
			Expect(leakOf(ctx, result(res).ID)).To(HaveKeyWithValue("reasons", ConsistOf("taint")))
		})

		It("parks a Trusted call for a person when the check fails", func(ctx SpecContext) {
			failure = errors.New("koinon-test: the guard is down")
			f := newFixture(ctx, trustedGrant)
			res, text := call(ctx, session(ctx, f.agentID), "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
			Expect(res.IsError).To(BeFalse(), text)
			Expect(result(res).Status).To(Equal("pending"))
			Expect(leakOf(ctx, result(res).ID)).To(HaveKeyWithValue("reasons", ConsistOf("error")))
		})

		It("does not check a call a person decides", func(ctx SpecContext) {
			f := newFixture(ctx, standardGrant)
			res, text := call(ctx, session(ctx, f.agentID), "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
			Expect(res.IsError).To(BeFalse(), text)
			Expect(result(res).Status).To(Equal("pending"))
			Expect(checked).To(BeEmpty())
		})

		It("answers a call already recorded with its stored decision, without a new check", func(ctx SpecContext) {
			f := newFixture(ctx, trustedGrant)
			s := session(ctx, f.agentID)
			first, _ := call(ctx, s, "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
			Expect(checked).To(HaveLen(1))
			answer = map[string]any{"leak": map[string]any{"reasons": []string{"taint"}}}
			again, text := call(ctx, s, "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
			Expect(again.IsError).To(BeFalse(), text)
			Expect(result(again).ID).To(Equal(result(first).ID))
			Expect(result(again).Status).To(Equal(result(first).Status))
			Expect(checked).To(HaveLen(1))
		})

		Describe("a call an approval scope covers", func() {
			scope := func(ctx context.Context, f fixture) {
				GinkgoHelper()
				// The scope's source is a call a person decides (the Standard preset).
				res, text := call(ctx, session(ctx, f.agentID), "comment_on_issue", map[string]any{"issue_number": 1, "body": "First."})
				Expect(res.IsError).To(BeFalse(), text)
				var source pgtype.UUID
				Expect(source.Scan(result(res).ID)).To(Succeed())
				_, err := q.CreateApprovalScope(ctx, store.CreateApprovalScopeParams{
					OrgID: f.org.ID, AgentID: f.agent.ID, Connector: "github", Tool: githubtools.ToolCommentOnIssue,
					Target: "github:acme/widgets", Kind: store.ApprovalScopeKindAgent,
					ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
					CreatedBy: f.agent.CreatedBy, SourceApprovalID: source,
					CallBinding: []byte(`{"connectionId":"00000000-0000-0000-0000-000000000000"}`),
				})
				Expect(err).NotTo(HaveOccurred())
				checked = nil
			}

			It("runs it when the check finds it clean", func(ctx SpecContext) {
				f := newFixture(ctx, standardGrant)
				scope(ctx, f)
				res, text := call(ctx, session(ctx, f.agentID), "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
				Expect(res.IsError).To(BeFalse(), text)
				Expect(result(res).Status).To(Equal("approved"))
				Expect(checked).To(HaveLen(1))
				Expect(checked[0].Tool).To(Equal(githubtools.ToolCommentOnIssue))
			})

			It("parks it for a person when the check holds it, and the scope does not cover it", func(ctx SpecContext) {
				answer = map[string]any{"leak": map[string]any{"reasons": []string{"both_legs"}}}
				f := newFixture(ctx, standardGrant)
				scope(ctx, f)
				res, text := call(ctx, session(ctx, f.agentID), "comment_on_issue", map[string]any{"issue_number": 3, "body": "Done."})
				Expect(res.IsError).To(BeFalse(), text)
				Expect(result(res).Status).To(Equal("pending"))
				Expect(decisionOf(ctx, result(res).ID)).To(Equal(store.ApprovalDecisionAsk))
				var scoped pgtype.UUID
				Expect(pool.QueryRow(ctx, `SELECT scope_id FROM approval_requests WHERE id = $1`, result(res).ID).Scan(&scoped)).To(Succeed())
				Expect(scoped.Valid).To(BeFalse())
				Expect(leakOf(ctx, result(res).ID)).To(HaveKeyWithValue("reasons", ConsistOf("both_legs")))
			})
		})
	})
})

const trustedGrant = `{"instructions":"help",` +
	`"grants":[{"connector":"github","scopes":["repo.read","pull_request.create","issue.comment"],"preset":"trusted"}],` +
	`"bindings":[{"connector":"github","connectionId":"00000000-0000-0000-0000-000000000000","targets":["acme/widgets"]}]}`

// decisionOf is the stored decision of an approval request.
func decisionOf(ctx context.Context, id string) store.ApprovalDecision {
	GinkgoHelper()
	var d store.ApprovalDecision
	Expect(pool.QueryRow(ctx, `SELECT decision FROM approval_requests WHERE id = $1`, id).Scan(&d)).To(Succeed())
	return d
}

func validPRMap() map[string]any {
	return map[string]any{
		"branch": "koinon/fix-typo", "title": "Fix a typo",
		"files": []map[string]any{{"path": "README.md", "content": "# Widgets\n"}},
	}
}

var _ = Describe("ValidatePullRequest with a connection's CI directories", func() {
	It("refuses the connection's configured directory as well as the provider's", func() {
		g, err := codehost.GuardOf(codehost.Capabilities{CIPaths: []string{".gitea/workflows/"}}, []byte(`{"ciDirs":["ci/pipelines"]}`))
		Expect(err).NotTo(HaveOccurred())
		for _, p := range []string{".gitea/workflows/ci.yml", "ci/pipelines/build.yml"} {
			a := validPR()
			a.Files = append(a.Files, file(p, "on: push"))
			Expect(githubtools.ValidatePullRequest(a, g)).To(MatchError(ContainSubstring("Koinon never proposes CI changes")), p)
		}
		a := validPR()
		a.Files = append(a.Files, file("ci/other.yml", "x"), file(".github/workflows/ok.yml", "x"))
		Expect(githubtools.ValidatePullRequest(a, g)).To(Succeed())
	})
})

var _ = Describe("the branch namespace", func() {
	const id = "01234567-89ab-cdef-0123-456789abcdef"

	DescribeTable("AgentNamespace",
		func(name, want string) { Expect(githubtools.AgentNamespace(name, id)).To(Equal(want)) },
		Entry("slugifies the display name", "Luna the Helper!", "luna-the-helper-89abcdef"),
		Entry("falls back when nothing is left", "???", "agent-89abcdef"),
		Entry("drops non-ASCII letters", "Lüna 月", "l-na-89abcdef"),
		Entry("is bounded", "an agent with a very long display name indeed", "an-agent-with-a-very-lon-89abcdef"),
	)

	DescribeTable("FinalBranch",
		func(suggestion, want string) {
			got, err := githubtools.FinalBranch("luna-01234567", suggestion)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("adds the namespace", "upgrade-x", "koinon/luna-01234567/upgrade-x"),
		Entry("replaces the koinon/ the agent added", "koinon/upgrade-x", "koinon/luna-01234567/upgrade-x"),
		Entry("keeps a nested suggestion", "deps/upgrade-x", "koinon/luna-01234567/deps/upgrade-x"),
	)

	DescribeTable("FinalBranch refuses",
		func(suggestion string) {
			_, err := githubtools.FinalBranch("luna-01234567", suggestion)
			Expect(err).To(MatchError(ContainSubstring("short name for the change")))
		},
		Entry("an empty name", ""),
		Entry("the bare prefix", "koinon/"),
		Entry("a bad ref", "a..b"),
		Entry("a .lock ref", "x.lock"),
		Entry("a space", "a b"),
		Entry("a reflog expression", "x@{1}"),
		Entry("non-ASCII", "ünïcode"),
		Entry("an empty segment", "/x"),
		Entry("a trailing slash", "x/"),
		Entry("a trailing dot", "x."),
		Entry("a hidden segment", "deps/.x"),
		Entry("a name too long for a ref", strings.Repeat("x", 200)),
	)
})
