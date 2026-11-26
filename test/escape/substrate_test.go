// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/guestauth"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// jwtShape matches the three base64url parts of a JWT whose header and payload are JSON objects
// ("eyJ" is `{"`). The probe reports paths, never the bytes it matched.
const jwtShape = `eyJ[A-Za-z0-9_-]{6,}\.eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`

// jwtClaims reads the payload of a compact JWS without checking its signature: the specs below look at
// what a token says, and the verifiers' own tests check the signature.
func jwtClaims(token string) map[string]any {
	GinkgoHelper()
	parts := strings.Split(token, ".")
	Expect(parts).To(HaveLen(3), "a compact JWS has three parts")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	Expect(err).NotTo(HaveOccurred())
	var c map[string]any
	Expect(json.Unmarshal(raw, &c)).To(Succeed())
	return c
}

// The identity tokens of Substrate, checked where they are minted. These specs need no cluster: they
// pin the two lifetimes and audiences that ADR 0120 sections 1.2 and 3.3 give, so a change to either
// fails the escape suite, not only a unit test. The specs that look for the tokens inside a guest are
// in the next container.
var _ = Describe("Substrate identity tokens", Label("escape", "substrate", "contract"), func() {
	var minter *actortoken.Minter
	var verifier *actortoken.Verifier

	BeforeEach(func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		minter, err = actortoken.NewMinter("koinon-escape", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		verifier, err = actortoken.NewVerifier("koinon-escape", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
	})

	It("gives the actor token the gateway as its only audience and a lifetime of 300 to 3600 seconds", func() {
		actor := actortoken.Actor{Org: "org-a", Agent: "agent-1", Run: "run-1"}
		for _, ttl := range []time.Duration{0, actortoken.MinTTL, actortoken.MaxTTL} {
			tok, err := minter.Mint(actor, ttl)
			Expect(err).NotTo(HaveOccurred(), ttl.String())
			c := jwtClaims(tok)
			Expect(c["aud"]).To(Equal("koinon-gateway"), "the audience is the gateway alone, never the actor")
			life := time.Duration(c["exp"].(float64)-c["iat"].(float64)) * time.Second
			Expect(life).To(BeNumerically(">=", 300*time.Second))
			Expect(life).To(BeNumerically("<=", 3600*time.Second))
			claims, err := verifier.Verify(tok)
			Expect(err).NotTo(HaveOccurred())
			Expect(claims.Actor).To(Equal(actor))
		}
		By("there is no way to mint a token outside the range")
		for _, ttl := range []time.Duration{time.Second, actortoken.MinTTL - time.Second, actortoken.MaxTTL + time.Second, 24 * time.Hour} {
			_, err := minter.Mint(actor, ttl)
			Expect(err).To(MatchError(actortoken.ErrTTL), ttl.String())
		}
	})

	It("refuses an actor token that is expired, or that another key signed", func() {
		actor := actortoken.Actor{Org: "org-a", Agent: "agent-1"}
		past := minter.WithClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
		old, err := past.Mint(actor, actortoken.MinTTL)
		Expect(err).NotTo(HaveOccurred())
		_, err = verifier.Verify(old)
		Expect(err).To(MatchError(actortoken.ErrInvalid), "an expired token")

		_, other, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		forger, err := actortoken.NewMinter("koinon-escape", "k1", other)
		Expect(err).NotTo(HaveOccurred())
		forged, err := forger.Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		_, err = verifier.Verify(forged)
		Expect(err).To(MatchError(actortoken.ErrInvalid), "a token under the id of a key it was not signed with")
	})

	It("gives the guest API token the actor as its audience and a lifetime of 60 seconds, and pins the first actor", func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		signer, err := guestauth.NewSigner(priv)
		Expect(err).NotTo(HaveOccurred())
		Expect(guestauth.TokenTTL).To(Equal(60 * time.Second))

		tokA, err := signer.Sign("org-a/actor-1", "")
		Expect(err).NotTo(HaveOccurred())
		c := jwtClaims(tokA)
		Expect(c["aud"]).To(Equal(guestauth.Audience("org-a/actor-1")))
		Expect(c["exp"].(float64) - c["iat"].(float64)).To(BeEquivalentTo(60))

		v, err := guestauth.NewVerifier([]ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		Expect(v.Verify(tokA)).To(Succeed(), "control: the first call is accepted")

		By("a token for another actor is refused by the front of this one: another org's token is no key to this guest")
		tokB, err := signer.Sign("org-b/actor-9", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(v.Verify(tokB)).NotTo(Succeed())

		By("a token under another key is refused")
		_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		other, err := guestauth.NewSigner(otherPriv)
		Expect(err).NotTo(HaveOccurred())
		forged, err := other.Sign("org-a/actor-1", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(v.Verify(forged)).NotTo(Succeed())
	})
})

// scanRootsGuest are the directories the credential scan of a Substrate guest walks. The guest has no
// policy that lists the readable roots, so the run names them (KOINON_ESCAPE_SCAN_ROOTS, comma-separated,
// replaces the list).
func scanRootsGuest() []string {
	if l := splitList(os.Getenv("KOINON_ESCAPE_SCAN_ROOTS")); len(l) > 0 {
		return l
	}
	return []string{"/etc", "/home", "/root", "/run", "/var", "/srv", "/mnt", "/opt", "/tmp", "/dev/shm", "/sandbox", "/workspace"}
}

// denial names are the results of a connect that the sandbox or the network refused. "open" is the
// only result that fails a spec; a name that is not here fails it too, so a new way of failing is read
// before it counts as a denial.
var denial = []any{"EACCES", "EPERM", "ENETUNREACH", "EHOSTUNREACH", "ECONNREFUSED", "ECONNRESET", "timeout", "gaierror"}

// The driver-neutral threats of ADR 0025 and ADR 0120, on a Substrate sandbox of the tier under test.
// Each mirrors a spec of "Agent sandbox" (sandbox_test.go); the OpenShell ones need providers and a
// per-binary policy that Substrate does not have, so the threat is restated without them: on Substrate
// egress is the gateway's, and a sandbox reaches nothing it was not given.
var _ = Describe("Substrate sandbox", Label("escape", "substrate", "live"), Ordered, ContinueOnFailure, func() {
	var name string

	probe := func(ctx context.Context, checks ...check) map[string]any {
		GinkgoHelper()
		return rig.probeIn(ctx, rig.workspace, name, checks...)
	}
	denied := func(ctx context.Context, ts []target) {
		GinkgoHelper()
		checks := make([]check, 0, len(ts))
		for _, t := range ts {
			checks = append(checks, tcp2(t))
		}
		res := probe(ctx, checks...)
		for _, c := range checks {
			Expect(res[c["id"].(string)]).To(BeElementOf(denial...), c["id"])
		}
	}

	BeforeAll(func(ctx SpecContext) {
		useSubstrate("the specs below are the Substrate restatement of the OpenShell ones")
		name = probeSandbox
	})

	It("runs on the tier's runtime, not on the host kernel", func(ctx SpecContext) {
		res := probe(ctx, check{"id": "kernel", "op": "kernel"})
		k := res["kernel"].(map[string]any)
		GinkgoWriter.Printf("tier %s, guest kernel release %v, hypervisor flag %v\n", rig.tier, k["release"], k["hypervisor"])
		// The nodes' kernel: uname -r on the worker node. The qualifying run sets it.
		testenv.Require(testenv.Var("host-kernel", "KOINON_ESCAPE_HOST_KERNEL"))
		Expect(k["release"]).NotTo(Equal(os.Getenv("KOINON_ESCAPE_HOST_KERNEL")), "the sandbox runs on the host kernel")
		if rig.tier == harness.TierVM {
			Expect(k["hypervisor"]).To(BeTrue(), "the guest of a microVM sees a hypervisor")
		}
	}, NodeTimeout(2*time.Minute))

	// ADR 0075 decision 9: the spec of a microVM guest is its own result, "not applicable", on every
	// other tier, and is never reported as passed there. It is a Skip whose message the report reads.
	It("runs in a microVM guest, not on the host kernel", func(ctx SpecContext) {
		if rig.tier != harness.TierVM {
			Skip(notApplicable + "tier " + string(rig.tier) + ": its sandbox is a user-space-kernel or container sandbox, not a microVM (ADR 0075 decision 9); the vm tier is qualified in #1349")
		}
		testenv.Require(testenv.Var("host-kernel", "KOINON_ESCAPE_HOST_KERNEL"))
		res := probe(ctx, check{"id": "kernel", "op": "kernel"})
		k := res["kernel"].(map[string]any)
		Expect(k["release"]).NotTo(Equal(os.Getenv("KOINON_ESCAPE_HOST_KERNEL")))
		Expect(k["hypervisor"]).To(BeTrue())
	}, NodeTimeout(2*time.Minute))

	It("denies direct egress to hosts and IPs outside what the gateway gives it", func(ctx SpecContext) {
		denied(ctx, targets(envOr("KOINON_ESCAPE_EGRESS",
			"1.1.1.1:443,8.8.8.8:443,example.com:443,example.com:80,[2606:4700:4700::1111]:443,github.com:22")))
	}, NodeTimeout(3*time.Minute))

	It("denies cloud metadata endpoints", func(ctx SpecContext) {
		denied(ctx, targets(envOr("KOINON_ESCAPE_METADATA",
			"169.254.169.254:80,169.254.169.254:443,[fd00:ec2::254]:80,169.254.170.2:80")))
	}, NodeTimeout(2*time.Minute))

	It("holds no service-account token and cannot reach the API server or the platform's services", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "sa", "op": "read", "path": "/var/run/secrets/kubernetes.io/serviceaccount/token"},
			check{"id": "sa run", "op": "read", "path": "/run/secrets/kubernetes.io/serviceaccount/token"},
			check{"id": "mounts", "op": "mounts", "contains": []string{"serviceaccount", "kubernetes.io", "spire", "spiffe"}},
			check{"id": "files", "op": "find", "roots": scanRootsGuest(), "contains": []string{"/serviceaccount", "kubernetes.io", "/.kube", "/kubeconfig"}, "segments": []string{"spire", "spiffe", "spire-agent", "agent.sock", "spiffe-workload-api"}},
			check{"id": "env", "op": "env"},
		)
		Expect(res["sa"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["sa run"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["mounts"]).To(BeEmpty(), "mounts that look like Kubernetes secret volumes or a SPIFFE socket")
		files := res["files"].(map[string]any)
		Expect(files["visited"]).To(BeNumerically(">", 100), "the walk read too few entries to mean anything")
		Expect(files["found"]).To(BeEmpty(), "files that look like Kubernetes credentials or a SPIFFE socket")
		Expect(res["env"]).NotTo(HaveKey(HavePrefix("KUBERNETES_")))

		By("the API server and the platform services are not reachable by Service IP")
		testenv.Require(testenv.Vars("escape-cluster-ips", "KOINON_ESCAPE_API", "KOINON_ESCAPE_INTERNAL"))
		denied(ctx, append(targets(os.Getenv("KOINON_ESCAPE_API")), targets(os.Getenv("KOINON_ESCAPE_INTERNAL"))...))
	}, NodeTimeout(4*time.Minute))

	// The credential check of the suite, restated: there is no credential in a Substrate guest at all
	// (ADR 0120 section 1), not even a placeholder, and the one value the execution layer injects, the
	// actor identity token, is minted and injected outside the guest. Its shape (a JWT) must be found
	// nowhere the agent user can read: files, environment, command lines of every process.
	It("holds no credential and no JWT-shaped value that it could read", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "scan", "op": "scan", "roots": scanRootsGuest(), "pattern": jwtShape},
			check{"id": "env", "op": "env"},
		)
		scan := res["scan"].(map[string]any)
		GinkgoWriter.Printf("scanned %v files\n", scan["files"])
		Expect(scan["files"]).To(BeNumerically(">", 100), "the scan read too few files to mean anything")
		Expect(scan["found"]).To(BeEmpty(), "a JWT-shaped value in the sandbox")
		for name, v := range res["env"].(map[string]any) {
			Expect(credentialName.MatchString(name)).To(BeFalse(), "credential-like variable %s", name)
			Expect(v).NotTo(MatchRegexp(jwtShape), name)
			Expect(name).NotTo(Equal(guestauth.VerifyKeysEnv), "the guest front removes its key variable before it starts a process")
		}
	}, NodeTimeout(4*time.Minute))

	// The actor token of ADR 0120 section 1.2 (issue #1386) is never in a Substrate guest: atenet-egress adds
	// it outside the guest. The specs of "Substrate actor identity" (substrate_identity_test.go) show that,
	// with the controls (a) to (h) of the amendment of 2026-11-26.

	It("runs the agent without capabilities, with no_new_privs and a seccomp filter", func(ctx SpecContext) {
		expectNoPrivileges(ctx, probe)
	}, NodeTimeout(2*time.Minute))

	// Landlock is best effort on the gVisor tiers (ADR 0120 section 4, issue #1150): gVisor has no Landlock,
	// and the root stays writable by decision. A kernel without it makes the write half "not applicable",
	// with that reason in the report, and the syscall half runs either way. The vm tier requires Landlock.
	It("denies writes outside Landlock's read-write list, and privileged syscalls", func(ctx SpecContext) {
		abi := probe(ctx, check{"id": "abi", "op": "syscall", "name": "landlock_abi"})["abi"]
		GinkgoWriter.Printf("landlock ABI: %v\n", abi)
		if _, ok := abi.(float64); ok {
			expectWritesDenied(ctx, probe)
			return
		}
		Expect(rig.tier).NotTo(Equal(harness.TierVM), "the vm tier requires Landlock; the guest answered %v", abi)
		expectSyscallsDenied(ctx, probe)
		Skip(notApplicable + fmt.Sprintf("tier %s (the write half): the kernel has no Landlock (%v); the root is writable by decision (ADR 0120 section 4, issue #1150). The syscall half passed", rig.tier, abi))
	}, NodeTimeout(2*time.Minute))

	It("cannot reach the guest agent or guest server as the agent user", func(ctx SpecContext) {
		expectGuestAgentClosed(ctx, probe, true)
	}, NodeTimeout(2*time.Minute))

	// A hardened-tier sandbox shares the worker node with other orgs' sandboxes by design (ADR 0075
	// decision 6): isolation between them is the sandbox boundary, the identity and the network. The two
	// specs below prove the network half and the driver half.
	Describe("another org's actor", func() {
		It("cannot be reached from this actor, though both run on the same worker node", func(ctx SpecContext) {
			// The qualifying run uses a worker pool of one node (docs/dev/escape-substrate.md), so both
			// actors are on it. The second actor listens on 8099 in the atespace of org B.
			other := fmt.Sprintf("kesb-%d", suffix())
			spec := rig.sandboxSpec(other)
			_, err := rig.driver.CreateSandbox(ctx, rig.otherWorkspace, spec)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) error {
				_, err := rig.driver.DeleteSandbox(ctx, rig.otherWorkspace, other)
				return err
			})
			Eventually(func(g Gomega) {
				sb, err := rig.driver.GetSandbox(ctx, rig.otherWorkspace, other)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sb.Phase).To(Equal(execution.PhaseReady), "%s: %s", sb.Reason, sb.Message)
			}).WithContext(ctx).WithTimeout(racescale.D(5 * time.Minute)).WithPolling(2 * time.Second).Should(Succeed())
			// The front stays PID 1 (a template Command would replace it, issue #1092): the listener is a
			// background process that Exec starts.
			_, stderr0, code0 := rig.exec(ctx, rig.otherWorkspace, other, "", "sh", "-c",
				"setsid nohup python3 -m http.server 8099 --bind 0.0.0.0 >/dev/null 2>&1 </dev/null &")
			Expect(code0).To(Equal(0), stderr0)
			Eventually(func() any {
				return rig.probeIn(ctx, rig.otherWorkspace, other, check{"id": "local", "op": "tcp", "host": "127.0.0.1", "port": 8099})["local"]
			}).WithContext(ctx).WithTimeout(racescale.D(90*time.Second)).WithPolling(2*time.Second).Should(Equal("open"), "control: the listener of org B is up")

			out, stderr, code := rig.exec(ctx, rig.otherWorkspace, other, "", "python3", "-c", "import socket;print(socket.gethostbyname(socket.gethostname()))")
			Expect(code).To(Equal(0), stderr)
			addr, err := netip.ParseAddr(strings.TrimSpace(out))
			Expect(err).NotTo(HaveOccurred(), "the address of org B's actor: %q", out)
			Expect(addr.IsLoopback()).To(BeFalse(), "an address that org A could route to, not loopback")

			By("controls: the address is org B's own, and its listener answers on that address")
			own, ownErr, code := rig.exec(ctx, rig.workspace, probeSandbox, "", "python3", "-c", "import socket;print(socket.gethostbyname(socket.gethostname()))")
			Expect(code).To(Equal(0), ownErr)
			Expect(strings.TrimSpace(own)).NotTo(Equal(addr.String()), "both actors share one address: a refused connect would prove nothing")
			Expect(rig.probeIn(ctx, rig.otherWorkspace, other, check{"id": "self", "op": "tcp", "host": addr.String(), "port": 8099})["self"]).To(Equal("open"),
				"org B cannot reach its own listener on the address that org A is tested against")
			obsA, err := rig.driver.Observe(ctx, rig.workspace, probeSandbox)
			Expect(err).NotTo(HaveOccurred())
			obsB, err := rig.driver.Observe(ctx, rig.otherWorkspace, other)
			Expect(err).NotTo(HaveOccurred())
			Expect(obsA.Worker).NotTo(BeEmpty())
			Expect(obsB.Worker).To(Equal(obsA.Worker), "the two actors must share a worker node (use a worker pool of one node); a denial across nodes is not the claim")

			By("org A gets no answer: a refusal would mean no listener, so it counts as a failure here")
			peer := tcp2(target{addr.String(), 8099})
			res := probe(ctx, peer)
			Expect(res[peer["id"].(string)]).To(BeElementOf("EACCES", "EPERM", "ENETUNREACH", "EHOSTUNREACH", "timeout"))
		}, NodeTimeout(10*time.Minute))

		It("is invisible to this org's driver calls: no get, no exec, no delete by name", func(ctx SpecContext) {
			// The driver scopes every call to one atespace. A name that exists only in org B is not found in
			// org A, so a bug that mixes up names cannot reach across.
			Expect(rig.otherWorkspace).NotTo(Equal(rig.workspace))
			mine := fmt.Sprintf("kesc-%d", suffix())
			theirs := rig.sandbox(ctx, rig.otherWorkspace, mine)
			_, err := rig.driver.GetSandbox(ctx, rig.workspace, theirs)
			Expect(err).To(MatchError(execution.ErrNotFound))
			_, err = rig.driver.Exec(ctx, rig.workspace, theirs, execution.ExecRequest{Command: []string{"true"}})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, execution.ErrNotFound) || errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "%v", err)
			out, err := rig.driver.DeleteSandbox(ctx, rig.workspace, theirs)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(execution.DeleteAbsent), "a delete in org A must not touch org B's sandbox")
			sb, err := rig.driver.GetSandbox(ctx, rig.otherWorkspace, theirs)
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseReady), "control: org B's sandbox is still there")
		}, NodeTimeout(10*time.Minute))
	})

	// ADR 0120 section 5 and ADR 0119 D5: a snapshot is pinned by a digest that the driver checks at
	// restore. A restore that names another digest, or another org's workspace, creates nothing.
	Describe("a restore", func() {
		It("is refused when the digest is wrong, and into another workspace", func(ctx SpecContext) {
			requireSnapshots()
			src := rig.sandbox(ctx, rig.workspace, fmt.Sprintf("kesr-%d", suffix()))
			snap, err := rig.driver.SnapshotSandbox(ctx, rig.workspace, src)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Digest).To(MatchRegexp(`^[a-z0-9]+:[0-9a-f]+$`), "the digest is <algorithm>:<hex>")

			bad := snap.SnapshotRef
			algo, _, _ := strings.Cut(snap.Digest, ":")
			bad.Digest = algo + ":" + strings.Repeat("0", 64)
			clone := fmt.Sprintf("kesx-%d", suffix())
			_, err = rig.driver.RestoreSandbox(ctx, rig.workspace, bad, rig.sandboxSpec(clone))
			Expect(err).To(MatchError(execution.ErrInvalid), "a restore with a wrong digest must be refused")
			_, err = rig.driver.GetSandbox(ctx, rig.workspace, clone)
			Expect(err).To(MatchError(execution.ErrNotFound), "the refused restore created a sandbox")

			By("a snapshot of one org does not restore into another org's workspace (ADR 0002)")
			_, err = rig.driver.RestoreSandbox(ctx, rig.otherWorkspace, snap.SnapshotRef, rig.sandboxSpec(clone))
			Expect(err).To(MatchError(execution.ErrNotFound))
			_, err = rig.driver.GetSandbox(ctx, rig.otherWorkspace, clone)
			Expect(err).To(MatchError(execution.ErrNotFound), "the refused restore created a sandbox")

			By("control: the right digest restores")
			restored, err := rig.driver.RestoreSandbox(ctx, rig.workspace, snap.SnapshotRef, rig.sandboxSpec(clone))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) error {
				_, err := rig.driver.DeleteSandbox(ctx, rig.workspace, restored.Name)
				return err
			})
			Eventually(func(g Gomega) {
				sb, err := rig.driver.GetSandbox(ctx, rig.workspace, restored.Name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sb.Phase).To(Equal(execution.PhaseReady), "%s: %s", sb.Reason, sb.Message)
			}).WithContext(ctx).WithTimeout(racescale.D(5 * time.Minute)).WithPolling(2 * time.Second).Should(Succeed())
		}, NodeTimeout(15*time.Minute))
	})
})

// tcp2 is a TCP connect check.
func tcp2(t target) check {
	return check{"id": "tcp " + t.String(), "op": "tcp", "host": t.host, "port": t.port}
}
