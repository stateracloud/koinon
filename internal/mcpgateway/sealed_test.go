// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/mcpcredfile"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// The gateway opens a v2 credential and fails closed (ADR 0097 decision 12, specs 3 and 4).
var _ = Describe("a sealed MCP credential", func() {
	const org = "11111111-1111-4111-8111-111111111111"
	const server = "22222222-2222-4222-8222-222222222222"
	var (
		gk      *secretstore.RecipientKey
		metrics *gatewaykeys.Metrics
		failed  func(reason string) float64
		agent   = gateway.AgentRef{Org: "acme", Name: "sealed-reader"}
	)

	parse := func(id string) pgtype.UUID {
		GinkgoHelper()
		var u pgtype.UUID
		Expect(u.Scan(id)).To(Succeed())
		return u
	}
	// seal makes the envelope the way koinon-api does, for the given binding.
	seal := func(to *secretstore.RecipientKey, orgID, serverID, table, column, plain string) secretstore.EnvelopeV2 {
		GinkgoHelper()
		env, err := secretstore.SealV2(to.Public(), secretstore.Binding{OrgID: orgID, Table: table, RowID: serverID, Column: column}, []byte(plain))
		Expect(err).NotTo(HaveOccurred())
		return env
	}
	// file writes env under the name the reconciler gives it.
	file := func(env secretstore.EnvelopeV2) string {
		GinkgoHelper()
		raw, err := json.Marshal(env)
		Expect(err).NotTo(HaveOccurred())
		name := mcpcredfile.SealedName(parse(org), parse(server), raw)
		p := filepath.Join(GinkgoT().TempDir(), name)
		Expect(os.WriteFile(p, raw, 0o600)).To(Succeed())
		return p
	}
	// run starts a gateway with the keyring, one target whose credential is credFile, and returns a session.
	run := func(ctx SpecContext, keys *secretstore.RecipientKeyring, credFile string) (*mcp.ClientSession, *upstreamLog, *auditLog) {
		GinkgoHelper()
		up := &upstreamLog{}
		srv := fakeUpstream("sealed", up)
		DeferCleanup(func() { srv.CloseClientConnections(); srv.Close() })
		audit := &auditLog{}
		gw := mcpgateway.New(mcpgateway.Options{
			Guard:       &recordingGuard{refuse: map[string]string{}},
			Audit:       audit,
			Credentials: mcpgateway.StaticCredentials{Keys: keys, OnOpenFailure: func(r string) { metrics.Failures.WithLabelValues(r).Inc() }},
		})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{
			Keys: []gateway.Key{key.Key},
			MCPState: gateway.MCPState{
				MCP:   []gateway.MCPTarget{{Name: "sealed", Org: "acme", URL: srv.URL + "/mcp", CredentialHeader: "X-Api-Key", CredentialFile: credFile}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "sealed", Tools: []string{"read_item"}}}},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		return connect(ctx, front.URL, key.Material), up, audit
	}

	BeforeEach(func() {
		var err error
		gk, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		metrics = gatewaykeys.NewMetrics(prometheus.NewRegistry())
		failed = func(reason string) float64 { return testutil.ToFloat64(metrics.Failures.WithLabelValues(reason)) }
	})

	It("opens with the gateway key and sets the header; the audit row names the key, not the value (spec 3)", func(ctx SpecContext) {
		env := seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
		s, up, audit := run(ctx, secretstore.NewRecipientKeyring(gk), file(env))
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "sealed_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse(), text(res))
		Expect(up.header("X-Api-Key")).To(HaveEach(fakeToken))
		Expect(audit.rows).NotTo(BeEmpty())
		Expect(audit.rows[len(audit.rows)-1].RecipientKeyID).To(Equal(gk.ID()))
		Expect(failed("open") + failed("unknown_key") + failed("format")).To(BeZero())
	})

	DescribeTable("fails closed: the call is refused, nothing goes upstream, and a fixed-label metric counts it (spec 4)",
		func(ctx SpecContext, reason string, make func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2)) {
			keys, env := make()
			s, up, audit := run(ctx, keys, file(env))
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "sealed_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsError).To(BeTrue())
			Expect(up.header("X-Api-Key")).To(BeEmpty(), "no credential and no request reaches the target")
			Expect(up.header("Mcp-Method")).NotTo(ContainElement("tools/call"))
			Expect(failed(reason)).To(BeNumerically(">=", 1))
			Expect(audit.rows[len(audit.rows)-1].CredKind).To(Equal(string(mcpgateway.CredentialError)))
			Expect(text(res)).NotTo(ContainSubstring(fakeToken))
		},
		Entry("an unknown key ID", gatewaykeys.ReasonUnknownKey, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			other, err := secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
			Expect(err).NotTo(HaveOccurred())
			return secretstore.NewRecipientKeyring(other), seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
		}),
		Entry("an empty keyring", gatewaykeys.ReasonUnknownKey, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			return secretstore.NewRecipientKeyring(), seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
		}),
		Entry("another row", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			return secretstore.NewRecipientKeyring(gk), seal(gk, org, "33333333-3333-4333-8333-333333333333", mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
		}),
		Entry("another org", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			return secretstore.NewRecipientKeyring(gk), seal(gk, "33333333-3333-4333-8333-333333333333", server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
		}),
		Entry("another table", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			return secretstore.NewRecipientKeyring(gk), seal(gk, org, server, "providers", mcpcredfile.CredentialColumn, fakeToken)
		}),
		Entry("another column", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			return secretstore.NewRecipientKeyring(gk), seal(gk, org, server, mcpcredfile.ServersTable, "name", fakeToken)
		}),
		Entry("a changed ciphertext", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			env := seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
			env.Ciphertext = flip(env.Ciphertext)
			return secretstore.NewRecipientKeyring(gk), env
		}),
		Entry("a changed nonce", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			env := seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
			env.Nonce = flip(env.Nonce)
			return secretstore.NewRecipientKeyring(gk), env
		}),
		Entry("a changed sealed DEK", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			env := seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
			env.SealedDEK = flip(env.SealedDEK)
			return secretstore.NewRecipientKeyring(gk), env
		}),
		Entry("a changed enc", gatewaykeys.ReasonOpen, func() (*secretstore.RecipientKeyring, secretstore.EnvelopeV2) {
			env := seal(gk, org, server, mcpcredfile.ServersTable, mcpcredfile.CredentialColumn, fakeToken)
			env.Enc = flip(env.Enc)
			return secretstore.NewRecipientKeyring(gk), env
		}),
	)

	It("refuses a v1 envelope in an mcpv2- file, and a file whose name has no binding", func(ctx SpecContext) {
		p := filepath.Join(GinkgoT().TempDir(), "mcpv2-bad-name")
		Expect(os.WriteFile(p, []byte(`{"v":1}`), 0o600)).To(Succeed())
		s, up, _ := run(ctx, secretstore.NewRecipientKeyring(gk), p)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "sealed_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(up.header("X-Api-Key")).To(BeEmpty())
		Expect(failed(gatewaykeys.ReasonFormat)).To(BeNumerically(">=", 1))
	})
})

func flip(b []byte) []byte {
	out := bytes.Clone(b)
	out[0] ^= 0xff
	return out
}
