// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// kindSource gives one credential of a fixed kind, as a source of that kind would.
type kindSource struct {
	kind  credkind.Kind
	value string
}

func (k kindSource) Credential(context.Context, gateway.MCPTarget, gateway.AgentRef) (mcpgateway.Credential, error) {
	return mcpgateway.Credential{Kind: k.kind, Header: "Authorization", Value: k.value}, nil
}

// fakePassport returns a JWT with the passport typ (ADR 0073) and fake claims and signature.
func fakePassport(n int) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"EdDSA","typ":"koinon-passport+jwt"}`) + "." +
		enc(fmt.Sprintf(`{"sub":"koinon_test_agent","jti":"koinon_test_%04d"}`, n)) + ".koinon_test_not-a-real-signature"
}

func echoResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func resultText(res *mcp.CallToolResult) string {
	Expect(res.Content).To(HaveLen(1))
	t, ok := res.Content[0].(*mcp.TextContent)
	Expect(ok).To(BeTrue())
	return t.Text
}

// The scrub covers every credential kind by the rule the one list gives it (#1221). One spec for each class, and
// one for a kind that is not in the list.
var _ = Describe("the credential scrub by kind (#1221)", func() {
	var (
		sc   *mcpgateway.CredentialScrub
		logs *bytes.Buffer
		log  *slog.Logger
	)
	target := gateway.MCPTarget{Name: "remote", Org: "acme", CredentialPrefix: "Bearer "}
	acme := gateway.AgentRef{Org: "acme", Name: "helper"}

	BeforeEach(func() {
		sc = mcpgateway.NewCredentialScrub()
		logs = &bytes.Buffer{}
		log = slog.New(sc.Handler(slog.NewJSONHandler(logs, nil)))
	})

	Context("class by-value", func() {
		for _, r := range credkind.Rules() {
			if r.Scrub != credkind.ByValue {
				continue
			}
			It(fmt.Sprintf("removes a value of kind %q from the results, the logs and the errors of its org", r.Kind), func(ctx SpecContext) {
				token := "koinon_test_not-a-real-token-" + string(r.Kind)
				sc.Record(r.Kind, "acme", "Bearer "+token)
				Expect(resultText(sc.Result("acme", echoResult("you sent Bearer "+token)))).
					To(Equal("you sent [redacted:secret]"))
				Expect(sc.TextFor("acme", "token="+token)).To(Equal("token=[redacted:secret]"))
				Expect(sc.TextFor("beta", token)).To(Equal(token), "another org's text is scrubbed with its own values only")
				log.InfoContext(ctx, "upstream said "+token, "echo", token)
				Expect(logs.String()).NotTo(ContainSubstring(token))
				Expect(logs.String()).To(ContainSubstring("[redacted:secret]"))
			})
		}

		It("also covers a value that a source of a by-value kind gives through the recording source", func(ctx SpecContext) {
			const token = "koinon_test_not-a-real-signin-token"
			src := sc.Source(kindSource{kind: credkind.MCPSignIn, value: "Bearer " + token})
			c, err := src.Credential(ctx, target, acme)
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Kind).To(Equal(credkind.MCPSignIn))
			Expect(resultText(sc.Result("acme", echoResult(token)))).To(Equal("[redacted:secret]"))
		})
	})

	Context("class by-shape (the passport)", func() {
		It("removes a passport from every org's results and from the logs once a passport was opened", func(ctx SpecContext) {
			p := fakePassport(1)
			src := sc.Source(kindSource{kind: credkind.Passport, value: "Bearer " + p})
			_, err := src.Credential(ctx, target, acme)
			Expect(err).NotTo(HaveOccurred())
			Expect(resultText(sc.Result("acme", echoResult("Authorization: Bearer "+p)))).
				To(Equal("Authorization: Bearer [redacted:secret]"))
			// A passport of another run is a passport too: the shape covers values the scrub never saw.
			other := fakePassport(2)
			Expect(resultText(sc.Result("beta", echoResult(other)))).To(Equal("[redacted:secret]"))
			Expect(sc.Text(`{"token":"` + other + `"}`)).To(Equal(`{"token":"[redacted:secret]"}`))
			log.InfoContext(ctx, "upstream said "+other, "echo", p)
			Expect(logs.String()).NotTo(ContainSubstring(p))
			Expect(logs.String()).NotTo(ContainSubstring(other))
			Expect(sc.Error(fmt.Errorf("upstream: %s", p))).To(MatchError("upstream: [redacted:secret]"))
		})

		It("keeps a JWT of another typ: the shape alone is not a credential", func() {
			sc.Record(credkind.Passport, "acme", fakePassport(1))
			enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
			idToken := enc(`{"alg":"RS256","typ":"JWT"}`) + "." + enc(`{"sub":"koinon_test"}`) + ".koinon_test_sig"
			res := echoResult("id_token " + idToken)
			Expect(resultText(sc.Result("acme", res))).To(Equal("id_token " + idToken))
		})

		It("keeps passports out of the capped value set, so they never push an org's other values out", func() {
			sc.MaxOrgValues = 10
			const static = "koinon_test_not-a-real-static-token"
			sc.Record(credkind.MCPStatic, "acme", static)
			for i := range 100 {
				sc.Record(credkind.Passport, "acme", "Bearer "+fakePassport(i))
			}
			Expect(sc.TextFor("acme", static)).To(Equal("[redacted:secret]"), "a passport flood evicted a static value")
		})

		It("records by value a value of a by-shape kind that does not have the shape", func() {
			const notJWT = "koinon_test_not-a-real-passport-value"
			sc.Record(credkind.Passport, "acme", notJWT)
			Expect(sc.TextFor("acme", notJWT)).To(Equal("[redacted:secret]"))
		})
	})

	Context("class no-value", func() {
		It("records nothing for a kind with no value, and still records a value given with it", func() {
			Expect(sc.Result("acme", nil)).To(BeNil())
			sc.Record(credkind.None, "acme")
			sc.Record(credkind.Error, "acme", "")
			res := echoResult("nothing to remove")
			Expect(sc.Result("acme", res)).To(BeIdenticalTo(res), "the fast path stays: no value and no shape")
			const stray = "koinon_test_not-a-real-stray-value"
			sc.Record(credkind.None, "acme", stray)
			Expect(sc.TextFor("acme", stray)).To(Equal("[redacted:secret]"), "a value is never dropped for its kind")
		})
	})

	Context("a kind that is not in the list", func() {
		for _, k := range []credkind.Kind{"koinon_test_future_kind", ""} {
			It(fmt.Sprintf("scrubs a value of kind %q by value", k), func(ctx SpecContext) {
				const token = "koinon_test_not-a-real-future-token"
				src := sc.Source(kindSource{kind: k, value: "Bearer " + token})
				_, err := src.Credential(ctx, target, acme)
				Expect(err).NotTo(HaveOccurred())
				Expect(resultText(sc.Result("acme", echoResult("echo "+token)))).To(Equal("echo [redacted:secret]"))
				log.InfoContext(ctx, "echo "+token)
				Expect(logs.String()).NotTo(ContainSubstring(token))
			})
		}
	})
})
