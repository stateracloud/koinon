// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The Substrate qualification harness (issue #1092). KOINON_ESCAPE_DRIVER=substrate points the suite at
// a real Agent Substrate and builds the driver the reconciler runs, from these variables. Nothing here
// runs on the OpenShell driver, and none of it needs a Kubernetes client: the driver speaks to ateapi
// and to the atenet router only.
//
//	KOINON_ESCAPE_SUBSTRATE_CONTROL        ateapi host:port (required)
//	KOINON_ESCAPE_SUBSTRATE_ROUTER         atenet router host:port, the TLS port 443 (required)
//	KOINON_ESCAPE_SUBSTRATE_ROUTER_CA      CA file of the router's certificate (default: ..._CA)
//	KOINON_ESCAPE_SUBSTRATE_ROUTER_SERVER_NAME  name checked against it (default: the host of the router address)
//	KOINON_ESCAPE_SUBSTRATE_ROUTER_CERT    client certificate bundle (key and chain, PEM) whose SPIFFE ID the
//	                                       router allows; the router refuses a client without one
//	KOINON_ESCAPE_SUBSTRATE_CA             CA file of ateapi's certificate (default: system roots)
//	KOINON_ESCAPE_SUBSTRATE_SERVER_NAME    name checked against ateapi's certificate
//	KOINON_ESCAPE_SUBSTRATE_TOKEN_FILE     ServiceAccount token (ateapi audience) sent as bearer token
//	KOINON_ESCAPE_SUBSTRATE_GUEST_KEY_FILE optional. Without it the run makes a key of its own: the driver
//	                                       writes the public half into the templates it creates, so the
//	                                       qualification never needs the production signing seed
//	KOINON_ESCAPE_SUBSTRATE_IMAGE          actor image with sh and python3 on its PATH, built on the
//	                                       sandbox-substrate overlay (guest front and launcher) (required)
//	KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS  the Substrate runtime class of the tier under test, for
//	                                       example the gVisor class of the install (required)
//	KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES comma-separated host:port gateway routes (required), for example
//	                                       koinon-gateway.koinon-system.svc:8080. The driver writes them into
//	                                       the egress policy of every sandbox (issue #1444, #1508): the only
//	                                       destinations that policy allows. A host is a lowercase DNS name,
//	                                       never an IP address. The suite sends one HTTP request to the first, as the positive control of the deny specs
//	KOINON_ESCAPE_TIER                     the isolation tier of that class: hardened (default), vm or dev
//	KOINON_ESCAPE_SUBSTRATE_IMAGE_MIRRORS  comma-separated host=target entries, as --substrate-image-mirrors
//	KOINON_ESCAPE_SUBSTRATE_IMAGE_REGISTRY registry that replaces the host of an image with no mirror entry
//	KOINON_ESCAPE_SUBSTRATE_WORKSPACE      the atespace of org A (default koinon-escape)
//	KOINON_ESCAPE_SUBSTRATE_OTHER_WORKSPACE the atespace of org B for the cross-org spec (default
//	                                       koinon-escape-other)
//	KOINON_ESCAPE_EGRESS_CONTROL           host:port that atenet-egress can reach (default example.com:80): the
//	                                       control of the egress gateway spec. Precondition: add it to
//	                                       workerNetworkPolicy.extraEgress or turn the chart policies off
//	                                       (docs/dev/escape-substrate.md)
//	KOINON_ESCAPE_EGRESS_REBIND            comma-separated host:port names that resolve to a metadata, loopback
//	                                       or unspecified address (default: sslip.io names, so the egress pod
//	                                       needs public DNS)
//	KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_ENDPOINT, _REGION, _BUCKET, _ACCESS_KEY_FILE, _SECRET_KEY_FILE
//	                                       the snapshot bucket (read-only key), as --substrate-snapshot-*;
//	                                       the restore spec needs them, and fails closed without
//	KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES comma-separated host:port gateway listeners that the driver writes
//	                                       into the egress policy of every actor, as the reconciler's model
//	                                       and MCP routes (issue #1444, required).
//	                                       Without routes the driver refuses every create
//	KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY host:port (a DNS name) of the identity stand-in that this
//	                                       process serves (substrate_identity_rig_test.go). The driver writes
//	                                       it as the code-host egress route, with the actor-token effect of
//	                                       issue #1386. atenet-egress must resolve it to this machine and reach
//	                                       it. The actor identity specs need it, and fail without it
//	KOINON_ESCAPE_SUBSTRATE_IDENTITY_LISTEN local listen address of the stand-in (default :<port of the above>)
//	KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER the issuer of the install's actor JWTs, as --actor-jwt-issuer
//	                                       (for example https://idp.ate-system.svc); this machine must reach it
//	KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_CA_FILE the CA that signs the issuer's certificate, as --actor-jwt-ca-file
//	KOINON_ESCAPE_SUBSTRATE_GATEWAY_DIRECT comma-separated host:port of the installed gateway's egress
//	                                       listener (its Service name, Service IP and pod IP): the spec that
//	                                       checks that no route skips atenet-egress connects to each
//
// The actor needs the sandbox-substrate overlay: the driver puts the guest launcher in front of every
// command, so an image without it cannot run a probe.

// substrateEnvNames are the variables the qualifying run must set; the harness fails, never skips, when
// a driver run lacks one, because a skipped qualification reads as a pass.
var substrateEnvNames = []string{
	"KOINON_ESCAPE_SUBSTRATE_CONTROL", "KOINON_ESCAPE_SUBSTRATE_ROUTER",
	"KOINON_ESCAPE_SUBSTRATE_IMAGE", "KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS",
	"KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES",
}

// missingSubstrateEnv names the required variables that are empty.
func missingSubstrateEnv(get func(string) string) []string {
	var missing []string
	for _, n := range substrateEnvNames {
		if strings.TrimSpace(get(n)) == "" {
			missing = append(missing, n)
		}
	}
	return missing
}

// substrateRig is the driver and the names a Substrate run uses.
type substrateRig struct {
	driver                    *substrate.Driver
	workspace, otherWorkspace string
	image, class              string
	tier                      harness.Tier
	snapshots                 bool
	// identity is the code-host egress route of the run: the identity stand-in, or nil when
	// KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY is not set. Only a sandbox with CodeHostEgress access
	// (SetGatewayAccess before the create) gets it in its egress policy.
	identity *substrate.GatewayRoute
}

// rig is the one rig of the run; the suite's BeforeSuite sets it when KOINON_ESCAPE_DRIVER=substrate.
var rig *substrateRig

// splitList splits a comma-separated list and drops empty entries.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseGatewayRoutes parses a comma-separated list of host:port gateway routes (issue #1508). It applies the
// checks of the driver (ValidateGatewayRoutes), so a bad input fails with a clear message before any call to
// Substrate. The list must not be empty: the driver refuses a sandbox without a route (issue #1444).
func parseGatewayRoutes(raw string) ([]substrate.GatewayRoute, error) {
	var routes []substrate.GatewayRoute
	for _, entry := range splitList(raw) {
		host, portText, err := net.SplitHostPort(entry)
		if err != nil {
			return nil, fmt.Errorf("KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES entry %q: want host:port: %w", entry, err)
		}
		port, err := strconv.ParseUint(portText, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES entry %q: the port is not a number in 1-65535", entry)
		}
		routes = append(routes, substrate.GatewayRoute{Host: host, Port: uint32(port)})
	}
	if err := substrate.ValidateGatewayRoutes(withListeners(routes), nil); err != nil {
		return nil, fmt.Errorf("KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES %q: %w", raw, err)
	}
	return routes, nil
}

// withListeners: the first route is the model route, every other route an MCP route. It gives each parsed route the header and audience of a gateway listener (issue #1489): the
// first route is the model listener, every other one an MCP listener. The specs never call these routes; the
// driver needs valid ones to write a policy.
func withListeners(routes []substrate.GatewayRoute) []substrate.GatewayRoute {
	out := make([]substrate.GatewayRoute, len(routes))
	for i, r := range routes {
		if i == 0 {
			out[i] = substrate.ModelRoute(r.Host, r.Port)
		} else {
			out[i] = substrate.MCPRoute(r.Host, r.Port)
		}
	}
	return out
}

// buildSubstrate builds the driver from the environment: the same settings type the binaries use, so
// the run qualifies the code path the reconciler takes.
func buildSubstrate(get func(string) string) (*substrate.Driver, *substrateRig, error) {
	if m := missingSubstrateEnv(get); len(m) > 0 {
		return nil, nil, fmt.Errorf("KOINON_ESCAPE_DRIVER=substrate needs %s", strings.Join(m, ", "))
	}
	tier := harness.Tier(get("KOINON_ESCAPE_TIER"))
	if tier == "" {
		tier = harness.TierHardened
	}
	class := strings.TrimSpace(get("KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS"))
	keyFile := get("KOINON_ESCAPE_SUBSTRATE_GUEST_KEY_FILE")
	if keyFile == "" {
		// A key made for this run only: it signs the calls of the run and lives in a 0600 file that is
		// removed once the config has read it.
		dir, err := os.MkdirTemp("", "koinon-escape-guestkey-")
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, nil, err
		}
		keyFile = filepath.Join(dir, "seed")
		if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
			return nil, nil, err
		}
	}
	routes, err := parseGatewayRoutes(get("KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES"))
	if err != nil {
		return nil, nil, err
	}
	routes = withListeners(routes)
	// The code-host egress route is the identity stand-in, so atenet-egress adds the actor JWT on its way
	// there (issue #1386) and the identity specs can see what the gateway gets.
	var identity *substrate.GatewayRoute
	if id := strings.TrimSpace(get("KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY")); id != "" {
		r, err := parseGatewayRoutes(id)
		if err != nil || len(r) != 1 {
			return nil, nil, fmt.Errorf("KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY must be one host:port: %q", id)
		}
		e := substrate.EgressRoute(r[0].Host, r[0].Port)
		identity = &e
	}
	s := cli.Substrate{
		GatewayRoutes:       routes,
		CodeHostEgressRoute: identity,
		ControlAddress:      get("KOINON_ESCAPE_SUBSTRATE_CONTROL"),
		RouterAddress:       get("KOINON_ESCAPE_SUBSTRATE_ROUTER"),
		RouterCAFile:        get("KOINON_ESCAPE_SUBSTRATE_ROUTER_CA"),
		RouterServerName:    get("KOINON_ESCAPE_SUBSTRATE_ROUTER_SERVER_NAME"),
		RouterCertFile:      get("KOINON_ESCAPE_SUBSTRATE_ROUTER_CERT"),
		CAFile:              get("KOINON_ESCAPE_SUBSTRATE_CA"),
		ServerName:          get("KOINON_ESCAPE_SUBSTRATE_SERVER_NAME"),
		BearerTokenFile:     get("KOINON_ESCAPE_SUBSTRATE_TOKEN_FILE"),
		RuntimeClassTiers:   []string{class + "=" + string(tier)},
		ImageMirrors:        splitList(get("KOINON_ESCAPE_SUBSTRATE_IMAGE_MIRRORS")),
		ImageRegistry:       get("KOINON_ESCAPE_SUBSTRATE_IMAGE_REGISTRY"),
		GuestKeyFile:        keyFile,
		SnapshotStore: cli.SnapshotStore{
			Endpoint:      get("KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_ENDPOINT"),
			Region:        get("KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_REGION"),
			Bucket:        get("KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_BUCKET"),
			AccessKeyFile: get("KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_ACCESS_KEY_FILE"),
			SecretKeyFile: get("KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_SECRET_KEY_FILE"),
		},
	}
	cfg, err := s.DriverConfig(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
	if err != nil {
		return nil, nil, err
	}
	d, err := substrate.Dial(substrate.Endpoint{
		ControlAddr: s.ControlAddress, RouterAddr: s.RouterAddress,
		CAFile: s.CAFile, ServerName: s.ServerName, BearerTokenFile: s.BearerTokenFile,
		RouterCAFile: s.RouterCAFile, RouterServerName: s.RouterServerName, RouterCertFile: s.RouterCertFile,
	}, cfg)
	if err != nil {
		return nil, nil, err
	}
	other := strings.TrimSpace(get("KOINON_ESCAPE_SUBSTRATE_OTHER_WORKSPACE"))
	if other == "" {
		other = "koinon-escape-other"
	}
	ws := strings.TrimSpace(get("KOINON_ESCAPE_SUBSTRATE_WORKSPACE"))
	if ws == "" {
		ws = "koinon-escape"
	}
	return d, &substrateRig{
		driver: d, workspace: ws, otherWorkspace: other, image: get("KOINON_ESCAPE_SUBSTRATE_IMAGE"),
		class: class, tier: tier, snapshots: s.SnapshotStore.Endpoint != "", identity: identity,
	}, nil
}

// The suite builds the rig once and starts the shared probe sandbox of org A. A parallel run builds
// one per process, which is fine: the driver holds connections and no state of the run. A run that
// names the Substrate driver and cannot build it is a failed qualification, never a skip.
var _ = BeforeSuite(func(ctx SpecContext) {
	if escapeDriver() != "substrate" {
		return
	}
	d, r, err := buildSubstrate(os.Getenv)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(d.Close)
	for _, ws := range []string{r.workspace, r.otherWorkspace} {
		_, err := d.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred(), ws)
	}
	rig = r
	probeSandbox = r.sandbox(ctx, r.workspace, fmt.Sprintf("kesa-%d", suffix()))
	substrateProbe = r.probe
}, NodeTimeout(8*time.Minute))

// useSubstrate skips a spec that is not applicable on the OpenShell driver, and fails one that needs
// the rig when a Substrate run has none.
func useSubstrate(why string) {
	GinkgoHelper()
	if escapeDriver() != "substrate" {
		Skip("not applicable on the OpenShell driver: " + why)
	}
	Expect(rig).NotTo(BeNil(), "KOINON_ESCAPE_DRIVER=substrate, but the rig was not built")
}

// probeProgram is the program the probe runs: the checks, then testdata/probe.py.
func probeProgram(checks []check) string {
	GinkgoHelper()
	cfg, err := json.Marshal(checks)
	Expect(err).NotTo(HaveOccurred())
	return fmt.Sprintf("import json\nCHECKS = json.loads(bytes.fromhex(%q))\n%s", hex.EncodeToString(cfg), probeScript)
}

// sandboxSpec is a sandbox of the rig's tier. It sets no Command: the image ENTRYPOINT (the guest
// front) must stay PID 1, because Substrate takes a template Command as an override of ENTRYPOINT and
// CMD, and the driver then reaches no front (issue #1092). A spec that needs a process starts it
// through Exec.
// It carries the platform's default sizing, as the reconciler's sandbox spec does: Substrate refuses
// an actor template without resources (issue #1092).
func (r *substrateRig) sandboxSpec(name string) execution.SandboxSpec {
	def := sandboxres.DefaultPolicy().Default
	return execution.SandboxSpec{
		Name: name, Image: r.image, RuntimeClassName: r.class,
		CPURequest: def.CPU.Request, CPULimit: def.CPU.Limit,
		MemoryRequest: def.Memory.Request, MemoryLimit: def.Memory.Limit,
	}
}

// sandbox creates a sandbox in workspace, deletes it when the container ends, and waits until it is
// ready. It returns the sandbox name.
func (r *substrateRig) sandbox(ctx context.Context, workspace, name string) string {
	GinkgoHelper()
	_, err := r.driver.CreateSandbox(ctx, workspace, r.sandboxSpec(name))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error {
		_, err := r.driver.DeleteSandbox(ctx, workspace, name)
		return err
	})
	Eventually(func(g Gomega) {
		sb, err := r.driver.GetSandbox(ctx, workspace, name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(sb.Phase).To(Equal(execution.PhaseReady), "%s: %s", sb.Reason, sb.Message)
	}).WithContext(ctx).WithTimeout(racescale.D(5 * time.Minute)).WithPolling(2 * time.Second).Should(Succeed())
	return name
}

// exec runs a command in a sandbox through the driver (the launcher of the tier goes in front of it)
// and returns its output and exit code.
func (r *substrateRig) exec(ctx context.Context, workspace, name, stdin string, command ...string) (string, string, int) {
	GinkgoHelper()
	var stderr syncBuffer
	p, err := r.driver.Exec(ctx, workspace, name, execution.ExecRequest{Command: command, Stderr: &stderr})
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = p.Close() }()
	go func() {
		_, _ = io.WriteString(p.Stdin(), stdin)
		_ = p.Stdin().Close()
	}()
	out, err := io.ReadAll(p.Stdout())
	if err != nil && !errors.Is(err, io.EOF) {
		Expect(err).NotTo(HaveOccurred(), stderr.String())
	}
	code, err := p.Wait()
	Expect(err).NotTo(HaveOccurred(), stderr.String())
	return string(out), stderr.String(), code
}

// syncBuffer is a buffer that the driver's stderr pump may write while a spec reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// probeIn runs checks in the named sandbox of workspace and returns each check's result by id.
func (r *substrateRig) probeIn(ctx context.Context, workspace, name string, checks ...check) map[string]any {
	GinkgoHelper()
	out, stderr, code := r.exec(ctx, workspace, name, probeProgram(checks), "python3", "-")
	Expect(code).To(Equal(0), stderr)
	var res map[string]any
	Expect(json.Unmarshal([]byte(out), &res)).To(Succeed(), out)
	return res
}

// probeSandbox is the shared sandbox of org A that the actor probe runs in. A spec container that
// wants its own sandbox calls rig.sandbox.
var probeSandbox string

// probe runs checks in the shared probe sandbox; it is substrateProbe.
func (r *substrateRig) probe(ctx context.Context, checks ...check) map[string]any {
	GinkgoHelper()
	return r.probeIn(ctx, r.workspace, probeSandbox, checks...)
}

// requireSnapshots skips a spec that needs the snapshot bucket. It fails under
// KOINON_REQUIRE=substrate-snapshots, which make test-escape-substrate sets.
func requireSnapshots() {
	GinkgoHelper()
	if rig.snapshots {
		return
	}
	testenv.Require(testenv.Var("substrate-snapshots", "KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_ENDPOINT"))
}

// notApplicable starts the message of a Skip that is not a missing precondition: the threat does not
// exist on this tier or driver. The report keeps it apart from a skipped spec.
const notApplicable = "not applicable on "
