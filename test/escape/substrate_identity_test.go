// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// The actor identity of a Substrate sandbox (issue #1386, ADR 0120 amendment of 2026-11-26, its threat
// model, item 2 and its Proof list). The guest holds no actor token: its git configuration sends the
// public placeholder, and atenet-egress, outside the guest, puts the actor's own JWT in the actor token
// header on the clear-text code-host egress rule. The specs check that from both sides: what the guest
// can read, and what reaches the gateway (the identity stand-in, substrate_identity_rig_test.go, which
// runs the gateway's own egress code in this process).
//
// The controls (a) to (h) are the Proof list of the amendment. Each spec name starts with its letter,
// so the run's report names it. (g) is a ship gate: when it fails, #1386 does not ship.
//
// Precondition: atenet-egress must reach the stand-in (KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY), and
// this machine must reach the install's actor JWT issuer. docs/dev/escape-substrate.md says how.
//
// Serial: the stand-in listens on one port of this machine, so one process runs the container.
var _ = Describe("Substrate actor identity", Label("escape", "substrate", "live"), Serial, Ordered, ContinueOnFailure, func() {
	var (
		gw   *identityGateway
		name string
		self actortoken.SubstrateActor
		base string // the egress URL of the guests: http://<stand-in host:port>
	)

	// guestGet sends one request from the sandbox name of workspace ws to the stand-in, and returns
	// the result of the probe: a map with "status" and "refusal", or the name of an error.
	guestGet := func(ctx context.Context, ws, sb, probe string, headers [][]string, tls bool) any {
		GinkgoHelper()
		res := rig.probeIn(ctx, ws, sb, check{
			"id": "req", "op": "egress_http", "host": rig.identity.Host, "port": rig.identity.Port,
			"path": gw.path(probe), "headers": headers, "tls": tls,
		})
		return res["req"]
	}
	// reply reads the status and the refusal of a probe result. ok is false for an error name.
	reply := func(res any) (status int, refusal string, ok bool) {
		m, ok := res.(map[string]any)
		if !ok {
			return 0, "", false
		}
		return int(m["status"].(float64)), m["refusal"].(string), true
	}
	// arrived waits for the one request of probe at the stand-in and returns what the stand-in saw.
	arrived := func(probe string) arrival {
		GinkgoHelper()
		var a []arrival
		Eventually(func() []arrival {
			a = gw.seen(probe)
			return a
		}).WithTimeout(racescale.D(10*time.Second)).WithPolling(100*time.Millisecond).Should(HaveLen(1), "the request %s at the gateway", probe)
		return a[0]
	}
	// expectAccepted checks that the request of probe reached the gateway with exactly one value, a
	// verified JWT of actor k, and that the gateway accepted that identity.
	expectAccepted := func(res any, probe string, k actortoken.SubstrateActor) arrival {
		GinkgoHelper()
		status, refusal, ok := reply(res)
		Expect(ok).To(BeTrue(), "the request %s got no response: %v", probe, res)
		a := arrived(probe)
		Expect(a.Tokens).To(Equal(1), "actor token values at the gateway")
		Expect(a.Placeholder).To(BeFalse(), "the placeholder reached the gateway: atenet-egress did not replace it")
		Expect(a.Verify).To(BeEmpty(), "the value at the gateway is not a JWT of the install's issuer")
		Expect(a.Key).To(Equal(k), "the JWT names another actor")
		Expect(a.accepted()).To(BeTrue(), "the gateway refused the identity: %d %s", a.Status, a.Refusal)
		Expect(acceptedReply(status, refusal)).To(BeTrue(), "the guest got %d %q", status, refusal)
		return a
	}
	// newActor gives the sandbox name of workspace ws the code-host egress access, as the reconciler
	// does before the create, then creates it and waits until it is ready.
	newActor := func(ctx context.Context, ws, sb string) actortoken.SubstrateActor {
		GinkgoHelper()
		Expect(rig.driver.SetGatewayAccess(ctx, ws, sb, execution.GatewayAccess{CodeHostEgress: true})).To(Succeed())
		_, err := rig.driver.CreateSandbox(ctx, ws, rig.sandboxSpec(sb))
		Expect(err).NotTo(HaveOccurred())
		var got execution.Sandbox
		Eventually(func(g Gomega) {
			got, err = rig.driver.GetSandbox(ctx, ws, sb)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Phase).To(Equal(execution.PhaseReady), "%s: %s", got.Reason, got.Message)
		}).WithContext(ctx).WithTimeout(racescale.D(5 * time.Minute)).WithPolling(2 * time.Second).Should(Succeed())
		Expect(got.ID).NotTo(BeEmpty(), "the actor UID")
		return actortoken.SubstrateActor{Atespace: ws, Name: got.Name, UID: got.ID}
	}
	// deleteActor deletes the sandbox and waits until Substrate has no actor of that name.
	deleteActor := func(ctx context.Context, ws, sb string) {
		GinkgoHelper()
		_, err := rig.driver.DeleteSandbox(ctx, ws, sb)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := rig.driver.GetSandbox(ctx, ws, sb)
			return err
		}).WithContext(ctx).WithTimeout(racescale.D(5*time.Minute)).WithPolling(2*time.Second).
			Should(MatchError(execution.ErrNotFound), "the deleted actor %s", sb)
	}
	placeholder := [][]string{{route.TokenHeader, actortoken.Placeholder}}

	BeforeAll(func(ctx SpecContext) {
		useSubstrate("atenet-egress adds the actor identity on Agent Substrate only")
		gw = startIdentityGateway(ctx)
		base = "http://" + net.JoinHostPort(rig.identity.Host, strconv.Itoa(int(rig.identity.Port)))
		name = fmt.Sprintf("kesi-%d", suffix())
		self = newActor(ctx, rig.workspace, name)
		DeferCleanup(func(ctx context.Context) error {
			_, err := rig.driver.DeleteSandbox(ctx, rig.workspace, name)
			return err
		})
		gw.record(self)
	}, NodeTimeout(10*time.Minute))

	// The rewrite of the spec of the interim (ADR 0120 section 1.2): the guest has no token file, no
	// process in it can read a signing key, and its git configuration holds only the placeholder. The
	// control is a real git request with that configuration, which the gateway accepts.
	It("holds no actor token and no signing key, and its git configuration sends only the placeholder", func(ctx SpecContext) {
		loc := connectors.Location{Web: connectors.HostPort{Host: forgeHost, Port: 443}, Default: true}
		cfg := codehost.EgressPlaceholderGitConfig(base, loc)

		By("koinon-agentd's configuration of Agent Substrate holds the placeholder and nothing to include")
		Expect(string(cfg)).To(ContainSubstring(codehost.ActorTokenHeader + ": " + actortoken.Placeholder))
		Expect(string(cfg)).NotTo(ContainSubstring("[include]"), "a git configuration that includes a token file")
		Expect(string(cfg)).NotTo(MatchRegexp(jwtShape))
		_, stderr, code := rig.exec(ctx, rig.workspace, name, string(cfg),
			"/bin/sh", "-c", writeGitConfigScript, "sh", codehost.SandboxGitConfig[:strings.LastIndex(codehost.SandboxGitConfig, "/")], codehost.SandboxGitConfig)
		Expect(code).To(Equal(0), stderr)
		got, stderr, code := rig.exec(ctx, rig.workspace, name, "", "cat", codehost.SandboxGitConfig)
		Expect(code).To(Equal(0), stderr)
		Expect(got).To(Equal(string(cfg)), "the git configuration in the guest")

		By("control: git with that configuration reaches the gateway, and the gateway accepts the actor")
		probe := fmt.Sprintf("git-%d", suffix())
		if _, _, code := rig.exec(ctx, rig.workspace, name, "", "/bin/sh", "-c", "command -v git"); code == 0 {
			_, stderr, _ := rig.exec(ctx, rig.workspace, name, "", "env", "GIT_CONFIG_GLOBAL="+codehost.SandboxGitConfig,
				"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
				"git", "ls-remote", "https://"+forgeHost+forgePrefix+probe+"/repo.git")
			// git fails: the stand-in reaches no upstream. The request is what counts.
			GinkgoWriter.Printf("git ls-remote: %s\n", strings.TrimSpace(stderr))
			a := arrived(probe)
			Expect(a.Tokens).To(Equal(1), "actor token values of git's request at the gateway")
			Expect(a.Verify).To(BeEmpty(), "git's request reached the gateway without the actor's JWT")
			Expect(a.Key).To(Equal(self))
			Expect(a.accepted()).To(BeTrue(), "the gateway refused git's request: %d %s", a.Status, a.Refusal)
		} else {
			GinkgoWriter.Println("the image has no git; the probe sends git's header instead")
			expectAccepted(guestGet(ctx, rig.workspace, name, probe, placeholder, false), probe, self)
		}

		By("the guest has no token file, and no JWT or signing key that it could read")
		res := rig.probeIn(ctx, rig.workspace, name,
			check{"id": "dir", "op": "read", "path": codehost.SandboxEgressDir},
			check{"id": "file", "op": "read", "path": codehost.SandboxEgressToken},
			check{"id": "jwt", "op": "scan", "roots": scanRootsGuest(), "pattern": jwtShape},
			check{"id": "pem", "op": "scan", "roots": scanRootsGuest(), "pattern": privateKeyShape()},
			check{"id": "jwk", "op": "scan", "roots": scanRootsGuest(), "pattern": privateJWKShape},
			check{"id": "env", "op": "env"},
		)
		Expect(res["dir"]).To(Equal("ENOENT"), "the egress token directory %s", codehost.SandboxEgressDir)
		Expect(res["file"]).To(Equal("ENOENT"), "the egress token file %s", codehost.SandboxEgressToken)
		for _, id := range []string{"jwt", "pem", "jwk"} {
			scan := res[id].(map[string]any)
			Expect(scan["files"]).To(BeNumerically(">", 100), "the %s scan read too few files to mean anything", id)
			Expect(scan["found"]).To(BeEmpty(), "%s: a readable file, environment or command line holds it", id)
		}
		for k, v := range res["env"].(map[string]any) {
			Expect(v).NotTo(MatchRegexp(jwtShape), k)
		}
	}, NodeTimeout(8*time.Minute))

	It("(a) a request with the placeholder reaches the gateway with the actor's JWT, and the gateway accepts it", func(ctx SpecContext) {
		probe := fmt.Sprintf("a-%d", suffix())
		a := expectAccepted(guestGet(ctx, rig.workspace, name, probe, placeholder, false), probe, self)
		Expect(a.TLS).To(BeFalse())
	}, NodeTimeout(3*time.Minute))

	It("(b) a request without the actor token header reaches the gateway without a token, and gets 401", func(ctx SpecContext) {
		probe := fmt.Sprintf("b-%d", suffix())
		status, refusal, ok := reply(guestGet(ctx, rig.workspace, name, probe, nil, false))
		Expect(ok).To(BeTrue(), "the request got no response")
		a := arrived(probe)
		Expect(a.Tokens).To(BeZero(), "atenet-egress added a token to a request without the header")
		Expect(a.Status).To(Equal(401))
		Expect(a.Refusal).To(Equal(string(route.ReasonNoToken)))
		Expect([]any{status, refusal}).To(Equal([]any{401, string(route.ReasonNoToken)}), "what the guest got")
	}, NodeTimeout(3*time.Minute))

	It("(c) a JWT that the guest forged is replaced, not passed through", func(ctx SpecContext) {
		forged := forgedJWT(self)
		probe := fmt.Sprintf("c-%d", suffix())
		a := expectAccepted(guestGet(ctx, rig.workspace, name, probe, [][]string{{route.TokenHeader, forged}}, false), probe, self)
		Expect(a.SHA).NotTo(ContainElement(sha256Hex(forged)), "the forged JWT reached the gateway")
	}, NodeTimeout(3*time.Minute))

	It("(d) a request with two actor token values is refused, or reaches the gateway with one minted value", func(ctx SpecContext) {
		forged := forgedJWT(self)
		probe := fmt.Sprintf("d-%d", suffix())
		status, refusal, ok := reply(guestGet(ctx, rig.workspace, name, probe,
			[][]string{{route.TokenHeader, actortoken.Placeholder}, {route.TokenHeader, forged}}, false))
		Expect(ok).To(BeTrue(), "the request got no response")
		a := arrived(probe)
		Expect(a.SHA).NotTo(ContainElement(sha256Hex(forged)), "the forged JWT reached the gateway")
		if a.Tokens == 1 {
			GinkgoWriter.Println("atenet-egress replaced both values with one")
			Expect(a.Verify).To(BeEmpty())
			Expect(a.Key).To(Equal(self))
			Expect(a.accepted()).To(BeTrue(), "%d %s", a.Status, a.Refusal)
		} else {
			GinkgoWriter.Printf("%d values reached the gateway\n", a.Tokens)
			Expect([]any{a.Status, a.Refusal}).To(Equal([]any{401, string(route.ReasonBadToken)}), "the gateway took one of several values")
			Expect([]any{status, refusal}).To(Equal([]any{401, string(route.ReasonBadToken)}))
		}

		By("the gateway refuses two copies of a real token")
		tok, ok := gw.token(probe)
		if !ok {
			p := fmt.Sprintf("d1-%d", suffix())
			expectAccepted(guestGet(ctx, rig.workspace, name, p, placeholder, false), p, self)
			tok, ok = gw.token(p)
		}
		Expect(ok).To(BeTrue())
		r := gw.replay(tok, tok)
		Expect([]any{r.Status, r.Refusal}).To(Equal([]any{401, string(route.ReasonBadToken)}))
	}, NodeTimeout(4*time.Minute))

	// The direct path is the gateway's egress listener by Service name, Service IP and pod IP. The guest
	// cannot tell which fence stopped it (atenet-egress, which has no rule for the listener, or the
	// NetworkPolicy fence of the chart): the spec checks that the request never reaches the gateway's
	// egress code. The chart check of the NetworkPolicy (charts/AGENTS.md) proves that fence alone.
	It("(e) a direct connection to the gateway's egress listener, past atenet-egress, is blocked", func(ctx SpecContext) {
		direct := targets(envOr("KOINON_ESCAPE_SUBSTRATE_GATEWAY_DIRECT", ""))
		Expect(direct).NotTo(BeEmpty(), "KOINON_ESCAPE_SUBSTRATE_GATEWAY_DIRECT is not set: the gateway egress listener by Service name, Service IP and pod IP")
		checks := make([]check, 0, len(direct))
		for _, t := range direct {
			checks = append(checks, check{
				"id": t.String(), "op": "egress_http", "host": t.host, "port": t.port,
				"path": gw.path("e"), "headers": placeholder,
			})
		}
		res := rig.probeIn(ctx, rig.workspace, name, checks...)
		for _, c := range checks {
			id := c["id"].(string)
			if status, refusal, ok := reply(res[id]); ok {
				GinkgoWriter.Printf("%s: %d from a proxy\n", id, status)
				Expect(refusal).To(BeEmpty(), "%s: the request reached the gateway's egress code (%d %s)", id, status, refusal)
				continue
			}
			Expect(res[id]).To(BeElementOf(append(denial, "noresponse")...), id)
		}
	}, NodeTimeout(3*time.Minute))

	It("(f) a token of a deleted actor does not work for the actor that replaces it (403)", func(ctx SpecContext) {
		sb := fmt.Sprintf("kesi-f-%d", suffix())
		DeferCleanup(func(ctx context.Context) error {
			_, err := rig.driver.DeleteSandbox(ctx, rig.workspace, sb)
			if errors.Is(err, execution.ErrNotFound) {
				return nil
			}
			return err
		})
		old := newActor(ctx, rig.workspace, sb)
		gw.record(old)
		p1 := fmt.Sprintf("f1-%d", suffix())
		expectAccepted(guestGet(ctx, rig.workspace, sb, p1, placeholder, false), p1, old)
		t1, ok := gw.token(p1)
		Expect(ok).To(BeTrue())

		By("the actor is deleted, and the State drops it")
		deleteActor(ctx, rig.workspace, sb)
		gw.forget(old)
		r := gw.replay(t1)
		GinkgoWriter.Printf("the old token verifies: %t\n", r.Verify == "")
		Expect([]any{r.Status, r.Refusal}).To(Equal([]any{403, string(route.ReasonUnknownActor)}), "the token of the deleted actor")

		By("an actor of the same name replaces it")
		next := newActor(ctx, rig.workspace, sb)
		Expect(next.UID).NotTo(Equal(old.UID), "Substrate gave the new actor the old UID")
		gw.record(next)
		p2 := fmt.Sprintf("f2-%d", suffix())
		res := guestGet(ctx, rig.workspace, sb, p2, placeholder, false)
		a := arrived(p2)
		Expect(a.Key.UID).NotTo(Equal(old.UID), "atenet-egress sent the token of the deleted actor for the new one")
		expectAccepted(res, p2, next)

		By("the old token does not work for the new actor")
		r = gw.replay(t1)
		Expect([]any{r.Status, r.Refusal}).To(Equal([]any{403, string(route.ReasonUnknownActor)}), "the token of the deleted actor, with its replacement in the State")
	}, NodeTimeout(15*time.Minute))

	// ADR 0120 amendment for #1386, item 2: atenet-egress takes the actor of a request from the guest's
	// network identity, never from a header. A guest that sets the headers that carry a forwarded
	// identity (X-Forwarded-Client-Cert and the x-forwarded-* set) must get the same result as without
	// them: the same status, the same refusal and a JWT of the same actor.
	It("(g) SHIP GATE: forwarded-identity headers that the guest sets (XFCC, x-forwarded-*) do not change the identity", func(ctx SpecContext) {
		const gate = "SHIP GATE (ADR 0120 amendment for #1386, item 2; #1386 must not ship): "
		probe := fmt.Sprintf("g0-%d", suffix())
		baseRes := guestGet(ctx, rig.workspace, name, probe, placeholder, false)
		baseline := expectAccepted(baseRes, probe, self)
		bStatus, bRefusal, _ := reply(baseRes)

		spiffe := func(ws, actor string) string {
			return "spiffe://substrate-actor.local/worker-for-actor/" + ws + "/" + actor
		}
		xfcc := func(uri string) string { return "By=" + uri + ";Hash=00;URI=" + uri }
		other := "kesi-g-none-" + strconv.FormatInt(suffix(), 10)
		forwarded := [][]string{
			{"X-Forwarded-For", "10.0.0.1"},
			{"X-Forwarded-Proto", "https"},
			{"X-Forwarded-Host", rig.otherWorkspace + ".koinon-escape.invalid"},
			{"X-Forwarded-Port", "443"},
			{"Forwarded", "for=10.0.0.1;proto=https"},
		}
		variants := []struct {
			name    string
			headers [][]string
		}{
			{"XFCC of another actor of the workspace", [][]string{{"X-Forwarded-Client-Cert", xfcc(spiffe(rig.workspace, probeSandbox))}}},
			{"XFCC of an actor of another workspace", [][]string{{"X-Forwarded-Client-Cert", xfcc(spiffe(rig.otherWorkspace, other))}}},
			{"two XFCC values", [][]string{
				{"X-Forwarded-Client-Cert", xfcc(spiffe(rig.workspace, probeSandbox))},
				{"X-Forwarded-Client-Cert", xfcc(spiffe(rig.otherWorkspace, other))},
			}},
			{"the x-forwarded-* set", forwarded},
			{"XFCC and the x-forwarded-* set", append([][]string{{"X-Forwarded-Client-Cert", xfcc(spiffe(rig.otherWorkspace, other))}}, forwarded...)},
		}
		for i, v := range variants {
			By(v.name)
			p := fmt.Sprintf("g%d-%d", i+1, suffix())
			res := guestGet(ctx, rig.workspace, name, p, append(append([][]string{}, placeholder...), v.headers...), false)
			status, refusal, ok := reply(res)
			Expect(ok).To(BeTrue(), gate+"%s: the request got no response (%v); without the headers it got %d %q", v.name, res, bStatus, bRefusal)
			a := arrived(p)
			GinkgoWriter.Printf("%s: forwarded headers at the gateway: %v\n", v.name, a.Forwarded)
			Expect(a.Tokens).To(Equal(baseline.Tokens), gate+"%s: %d actor token values reached the gateway, without the headers %d", v.name, a.Tokens, baseline.Tokens)
			Expect(a.Verify).To(Equal(baseline.Verify), gate+"%s: the token check changed", v.name)
			Expect(a.Key).To(Equal(baseline.Key), gate+"%s: atenet-egress minted the JWT of another actor", v.name)
			Expect([]any{status, refusal}).To(Equal([]any{bStatus, bRefusal}), gate+"%s: the result changed", v.name)
			Expect([]any{a.Status, a.Refusal}).To(Equal([]any{baseline.Status, baseline.Refusal}), gate+"%s: the gateway's decision changed", v.name)
		}
	}, NodeTimeout(6*time.Minute))

	// The driver puts the actor JWT on the clear-text egress rule only (ADR 0120 amendment, item 3): no
	// https rule carries it. So an https request to the egress URL must never carry a token.
	It("(h) an https request to the egress URL gets no token", func(ctx SpecContext) {
		probe := fmt.Sprintf("h-%d", suffix())
		res := guestGet(ctx, rig.workspace, name, probe, placeholder, true)
		GinkgoWriter.Printf("https request: %v\n", res)
		if status, refusal, ok := reply(res); ok {
			Expect(acceptedReply(status, refusal)).To(BeFalse(), "the gateway accepted an https request")
		}
		Consistently(func() []arrival {
			var bad []arrival
			for _, a := range gw.seen(probe) {
				if a.Verify == "" || a.accepted() {
					bad = append(bad, a)
				}
			}
			return bad
		}).WithTimeout(racescale.D(3*time.Second)).WithPolling(200*time.Millisecond).Should(BeEmpty(), "an https request reached the gateway with a verified token")
		for _, a := range gw.seen(probe) {
			Expect(a.TLS).To(BeTrue(), "a request of the https probe arrived without TLS: something took the TLS off on the way")
		}
	}, NodeTimeout(3*time.Minute))

	// The audience and the lifetime of the real actor JWT (ADR 0120 section 1.2, amendment of 2026-11-27,
	// issue #1489), on a token that atenet-egress minted and the install's issuer signed: the contract
	// specs of "Substrate identity tokens" (substrate_test.go) check the verifier with a fake issuer, this
	// one checks what Substrate really mints. The token stays in this process.
	It("(i) the actor's JWT has the egress listener as its one audience and a lifetime of 300 to 3600 seconds, and is refused on the other listeners and after it expires", func(ctx SpecContext) {
		probe := fmt.Sprintf("i-%d", suffix())
		expectAccepted(guestGet(ctx, rig.workspace, name, probe, placeholder, false), probe, self)
		tok, ok := gw.token(probe)
		Expect(ok).To(BeTrue(), "the stand-in kept no verified token of the request")

		c := jwtClaims(tok)
		Expect(c["aud"]).To(Equal([]any{actortoken.AudienceEgress}), "the audience of the minted JWT")
		exp := time.Unix(int64(c["exp"].(float64)), 0)
		life := exp.Sub(time.Unix(int64(c["iat"].(float64)), 0))
		GinkgoWriter.Printf("minted actor JWT: lifetime %s, expires in %s\n", life, time.Until(exp).Round(time.Second))
		Expect(life).To(BeNumerically(">=", actortoken.MinTTL))
		Expect(life).To(BeNumerically("<=", actortoken.MaxTTL))

		By("control: the egress listener's verifier accepts the token now")
		got, err := gw.verifierFor(ctx, actortoken.AudienceEgress, nil).Verify(tok)
		Expect(err).NotTo(HaveOccurred())
		Expect(actortoken.SubstrateActor{Atespace: got.Atespace, Name: got.ActorName, UID: got.ActorUID}).To(Equal(self))

		By("the model and MCP listeners refuse the token: a token copied from an egress request is no key to them")
		for _, aud := range []string{actortoken.AudienceModel, actortoken.AudienceMCP} {
			_, err := gw.verifierFor(ctx, aud, nil).Verify(tok)
			Expect(err).To(MatchError(actortoken.ErrAudience), aud)
		}

		By("the egress listener refuses the token once it has expired")
		after := exp.Add(actortoken.SubstrateLeeway + time.Second)
		_, err = gw.verifierFor(ctx, actortoken.AudienceEgress, func() time.Time { return after }).Verify(tok)
		Expect(err).To(MatchError(actortoken.ErrExpired))

		By("the guest holds no JWT after the requests of this container")
		res := rig.probeIn(ctx, rig.workspace, name, check{"id": "jwt", "op": "scan", "roots": scanRootsGuest(), "pattern": jwtShape})
		scan := res["jwt"].(map[string]any)
		Expect(scan["files"]).To(BeNumerically(">", 100), "the scan read too few files to mean anything")
		Expect(scan["found"]).To(BeEmpty(), "a readable file, environment or command line holds a JWT")
	}, NodeTimeout(4*time.Minute))
})

// writeGitConfigScript is koinon-agentd's writeGitScript (internal/agentd/gitconfig.go): it replaces
// $2 with stdin in one rename, in the private directory $1. It is copied here because it is not
// exported.
const writeGitConfigScript = `umask 077 && mkdir -p "$1" && t=$(mktemp "$1/.git.XXXXXX") && cat > "$t" && rm -rf -- "$2" && mv -f -- "$t" "$2"`

// privateKeyShape matches a PEM private key of any type. It is built here, so the source holds no PEM
// header that a secret scanner reads as a key.
func privateKeyShape() string { return "-----" + "BEGIN [A-Z ]*PRIVATE KEY" + "-----" }

// privateJWKShape matches a JWK with a private member: a key type, then "d" in the same object.
const privateJWKShape = `"kty"\s*:\s*"(EC|RSA|OKP)"[^}]{0,2000}"d"\s*:\s*"`

// forgedJWT is a JWT that the guest makes for actor k: the claims of a real actor token and a signature
// that no key made. It is obviously fake.
func forgedJWT(k actortoken.SubstrateActor) string {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"ES256","typ":"JWT","kid":"koinon_test_not-a-real-key"}`))
	claims := enc(fmt.Appendf(nil, `{"aud":["koinon-gateway"],"sub":"koinon_test_not-a-real-subject","atespace":%q,"actor_name":%q,"actor_uid":%q,"exp":%d}`,
		k.Atespace, k.Name, k.UID, time.Now().Add(10*time.Minute).Unix()))
	return header + "." + claims + "." + enc([]byte("koinon_test_not-a-real-signature"))
}
