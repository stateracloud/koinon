// SPDX-License-Identifier: Apache-2.0

// Package substrate is the Agent Substrate driver of the execution seam (epic 1082, ADR 0119). It
// implements [execution.Compute], [execution.Suspender] and [execution.Snapshotter]; it has no
// [execution.Egress] half, because Substrate's atenet is default-deny and L7 rules and credentials
// belong to the Koinon gateway (ADR 0097).
//
// The mapping, with the version of Substrate it was written against (v1.0.1, protos in
// third_party/substrate):
//
//   - a workspace is an atespace (ADR 0002: an org is an atespace);
//   - a sandbox is an actor, created from an ActorTemplate that the driver derives from the
//     SandboxSpec and names by a hash of it, so equal specs share a template;
//   - StopSandbox and SuspendSandbox are one operation, SuspendActor: Substrate has no stopped
//     state that is not a suspended one. Both report [execution.PhaseSuspended];
//   - Exec has no control-API call (upstream issue 185), so it runs over the in-guest
//     ProcessService, and so does Upload (a tar stream on the stdin of a process), both through the
//     atenet router with the header ate-target-actor. The guest has no FileSystemService (#1478);
//   - a snapshot is a Substrate tag on a suspended actor, with a Koinon-side SHA-256 digest that
//     RestoreSandbox verifies (Substrate has no digest of its own; upstream T-25).
//
// Two behaviours of Substrate shape the code, both proven in the phase 0 lab run (ADR 0119,
// Verification): any request that reaches the router resumes a suspended actor, so the driver sends
// guest traffic only after it has read the actor as running from the control API, and before it
// suspends it drains its own calls and then verifies that the actor stayed suspended (see suspend);
// and a process's output is a spool that a client can read again from a
// byte offset, which is how a stream survives a suspend and resume without a lost line.
package substrate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
	"github.com/stateracloud/koinon/internal/guestauth"
	"github.com/stateracloud/koinon/internal/harness"
)

// TargetActorHeader selects the actor a request to the atenet router goes to, as
// "<atespace>/<actor>" (Substrate env client, internal/ate TargetActorHeader).
const TargetActorHeader = "ate-target-actor"

// Provisioner creates the org atespace and its access policy. The caller of the driver holds no right to
// create an atespace (ADR 0128): only the provisioner does.
type Provisioner interface {
	// EnsureAtespace makes sure the atespace name exists and that the caller may use it. It is idempotent.
	EnsureAtespace(ctx context.Context, name string) error
}

// Config tunes the driver. The zero value is usable: every field has a default.
type Config struct {
	// Provisioner, when set, creates the atespace of an org. EnsureWorkspace then never calls CreateAtespace:
	// the reconciler holds no global owner role (ADR 0128). Nil keeps the old behaviour, for a caller that is
	// itself a global owner (a test, a single-principal lab).
	Provisioner Provisioner
	// PollInterval is how long the driver waits between reads of an actor's state while it waits
	// for it to run. Default 250 ms; the wait backs off to eight times that.
	PollInterval time.Duration
	// StreamStall is how long an output stream may fail again and again without delivering a byte
	// before the process fails with [execution.ErrUnavailable]. Default 2 minutes.
	StreamStall time.Duration
	// GuestReadyTimeout is how long Exec and Upload retry a guest that is not reachable yet. An
	// actor is running before its guest daemon listens. Default 30 s.
	GuestReadyTimeout time.Duration
	// FrontCheckTimeout is how long CheckFront waits for the guest front to answer. The front is a local
	// process of the guest: a longer wait is a front that does not run. Default 5 s.
	FrontCheckTimeout time.Duration
	// SnapshotStorageLocation is the ActorTemplate's snapshot storage location, as Substrate's
	// install names it. Empty leaves the template's field unset, which Substrate's default fills.
	SnapshotStorageLocation string
	// GoldenSnapshots turns on Substrate's golden snapshot of each template. The zero value is off:
	// the template sets GOLDEN_SNAPSHOT_MODE_DISABLED (issue #1492). A golden clone copies memory from
	// one sandbox to another, which ADR 0120 section 5.2 forbids; true is an admin's exception to it.
	GoldenSnapshots bool
	// SnapshotFidelity is the fidelity templates ask for. The zero value is memory, which D5 wants
	// (disk and memory from the first release).
	SnapshotFidelity ateapiv1.SnapshotFidelity
	// ContentDigest returns a digest ("<hex>") of the bytes behind a snapshot's object-store
	// address. It is part of every snapshot digest, so a restore refuses a snapshot whose content
	// changed behind the same address (upstream T-25; ADR 0119 D5). It is required for snapshots:
	// when it is nil, SnapshotSandbox and RestoreSandbox fail closed with [execution.ErrUnsupported],
	// because a digest over the control API's metadata alone would restore a changed object without
	// error. [ObjectStore.ContentDigester] makes the production function; the reconciler builds it from
	// the --substrate-snapshot-* flags, and without them every snapshot fails closed.
	ContentDigest func(ctx context.Context, objectURI string) (string, error)
	// RuntimeClassTiers maps the runtime class of a sandbox (SandboxSpec.RuntimeClassName, which the
	// template carries as a worker selector) to its isolation tier (ADR 0075). Exec puts the launcher
	// prefix of that tier in front of every command ([harness.Descriptor.LauncherPrefix]).
	RuntimeClassTiers map[string]harness.Tier
	// DefaultTier is the tier of a sandbox whose template names no runtime class. Empty means there is
	// none. A sandbox whose tier is unknown fails Exec with [execution.ErrFailedPrecondition]: the
	// driver never starts a guest process, Upload's included, without the launcher.
	DefaultTier harness.Tier
	// ImageMirrors maps a registry host of an actor image (docker.io, ghcr.io, ...) to the image
	// source of the node agent, for example "cache.koinon.svc/ghcr.io". The path, the tag and the digest
	// of the reference are kept (see [RewriteImage]). The node agent reaches one image source only
	// (ADR 0120 section 6), so every actor image goes through here. The rewritten reference is part of
	// the template hash: a changed mirror name re-hashes every template.
	ImageMirrors map[string]string
	// ImageRegistry replaces the host of an image whose host has no entry in ImageMirrors.
	ImageRegistry string
	// DrainTimeout bounds each of the two waits of a suspend for the router calls that started
	// before it: first for the short calls (stdin writes, uploads) to end by themselves, then,
	// after they are cancelled, for them to return. It also bounds the wait for an actor that a
	// late request resumed to settle before the driver suspends it again. Default 10 s.
	DrainTimeout time.Duration
	// Logger gets one line per suspend that a late router request undid. Nil uses slog.Default().
	Logger *slog.Logger
	// GuestSigner signs every guest API call (internal/guestauth, issue #1309). Required: without it
	// every template render and every guest call fails with [execution.ErrFailedPrecondition]. Its
	// private key stays in the control plane; templates carry only the public key.
	GuestSigner *guestauth.Signer
	// GuestVerifyKeys are more public keys that templates give the guest front next to the signer's,
	// for a key rotation. They are part of the template hash.
	GuestVerifyKeys []ed25519.PublicKey
	// GatewayRoutes are the gateway listeners that every guest calls: the model and MCP listeners (issue
	// #1444). The driver writes them as the actor's egress policy before every create, restore, start
	// and resume, and the actor reaches nothing else. Required: without them each of these calls fails
	// with [execution.ErrFailedPrecondition]. Check them with [ValidateGatewayRoutes].
	GatewayRoutes []GatewayRoute
	// CodeHostEgressRoute is the gateway's egress listener, or nil when the install has none. Only an
	// actor whose [execution.GatewayAccess] has CodeHostEgress gets it in its policy
	// ([Driver.SetGatewayAccess]). It must not be one of GatewayRoutes: then every actor would reach
	// it. Check it with [ValidateGatewayRoutes].
	CodeHostEgressRoute *GatewayRoute
	// ActorNames is the durable record of the actor names that were sent to CreateActor (issue #1092).
	// CreateSandbox and RestoreSandbox claim each name in it first and refuse a name that was claimed
	// before with [execution.ErrNameReused]. Required: without it every create and restore fails with
	// [execution.ErrFailedPrecondition]. Production passes the database ledger of package actornames.
	ActorNames ActorNameLedger
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.StreamStall <= 0 {
		c.StreamStall = 2 * time.Minute
	}
	if c.GuestReadyTimeout <= 0 {
		c.GuestReadyTimeout = 30 * time.Second
	}
	if c.FrontCheckTimeout <= 0 {
		c.FrontCheckTimeout = 5 * time.Second
	}
	if c.SnapshotFidelity == ateapiv1.SnapshotFidelity_SNAPSHOT_FIDELITY_UNSPECIFIED {
		c.SnapshotFidelity = ateapiv1.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 10 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Driver is the Substrate implementation of the execution seam's compute half.
type Driver struct {
	control ateapiv1.ControlClient
	process ateenvv1alpha.ProcessServiceClient
	router  grpc.ClientConnInterface // signs every call (signedConn)
	// routerState reports the state of the router connection, or is nil when the connection has none to
	// report (a test double). routerDown reads it.
	routerState func() connectivity.State
	// routerSeen is set once the router connection was READY in this process. After that, a connection
	// failure is an outage that may pass, not an install error.
	routerSeen atomic.Bool
	// routerRefusal returns the identity refusal (TLS alert, x509 error) that a router call error carries, or
	// nil. It is nil when the connection is not a Dial connection.
	routerRefusal func(error) error
	cfg           Config
	closers       []io.Closer
	lineage       lineages

	mu    sync.Mutex
	gates map[key]*gate
	// owed holds the actors that SnapshotSandbox suspended and could not resume. A retry of the
	// snapshot resumes them, because the seam says the sandbox keeps running.
	owed map[key]bool
	// sealOwed holds the actors that a repoint changed and that did not get the synchronous suspend of the new
	// template yet (issue #1482). The next resume takes it. The mark lives in memory: a restart of the driver loses it.
	sealOwed map[key]bool
	// tiers caches the tier of a template by atespace and name. A template name is a hash of its
	// content, so the entry never goes stale.
	tiers map[key]harness.Tier
	// access holds the gateway access the reconciler set for each sandbox (SetGatewayAccess). It is not
	// stored anywhere else: a sandbox without an entry gets the zero access, so the driver fails closed
	// after a restart until the reconciler sets it again.
	access map[key]execution.GatewayAccess

	resuspends *prometheus.CounterVec
}

// The driver implements the compute half and both optional capabilities. It has no Egress half.
var (
	_ execution.Compute     = (*Driver)(nil)
	_ execution.Suspender   = (*Driver)(nil)
	_ execution.Snapshotter = (*Driver)(nil)

	_ execution.GatewayAccessSetter = (*Driver)(nil)
)

// key names an actor.
type key struct{ atespace, actor string }

func (k key) String() string { return k.atespace + "/" + k.actor }

// New returns a driver on existing connections: control reaches ateapi, router reaches the atenet
// router. The caller owns both.
//
// Every call through router carries a token from cfg.GuestSigner (issue #1309).
func New(control, router grpc.ClientConnInterface, cfg Config) *Driver {
	d := &Driver{
		control:    ateapiv1.NewControlClient(control),
		cfg:        cfg.withDefaults(),
		gates:      map[key]*gate{},
		owed:       map[key]bool{},
		sealOwed:   map[key]bool{},
		tiers:      map[key]harness.Tier{},
		access:     map[key]execution.GatewayAccess{},
		lineage:    lineages{m: map[string]lineage{}},
		resuspends: newResuspends(),
	}
	d.router = signedConn{cc: router, d: d}
	if cc, ok := router.(interface{ GetState() connectivity.State }); ok {
		d.routerState = cc.GetState
	}
	d.process = ateenvv1alpha.NewProcessServiceClient(d.router)
	return d
}

// Suspend outcomes, the only values of the outcome label of koinon_substrate_resuspends_total.
const (
	resuspendRetried = "retried" // a late router request resumed the actor; the driver suspended it again
	resuspendFailed  = "failed"  // the actor kept being resumed; the suspend returned an error
)

func newResuspends() *prometheus.CounterVec {
	v := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "koinon_substrate_resuspends_total",
		Help: "Suspends that found the actor resumed again by a late router request, by outcome (retried or failed). No sandbox labels: ADR 0023.",
	}, []string{"outcome"})
	v.WithLabelValues(resuspendRetried)
	v.WithLabelValues(resuspendFailed)
	return v
}

// Collectors returns the driver's own metrics, for the caller to register.
func (d *Driver) Collectors() []prometheus.Collector {
	return []prometheus.Collector{d.resuspends}
}

// setOwed records whether a snapshot owes k a resume.
func (d *Driver) setOwed(k key, owed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if owed {
		d.owed[k] = true
	} else {
		delete(d.owed, k)
	}
}

// setSealOwed records whether a repoint owes k a synchronous suspend of its new template.
func (d *Driver) setSealOwed(k key, owed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if owed {
		d.sealOwed[k] = true
	} else {
		delete(d.sealOwed, k)
	}
}

func (d *Driver) isSealOwed(k key) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sealOwed[k]
}

func (d *Driver) isOwed(k key) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.owed[k]
}

// Close releases the connections that Dial opened. It does nothing for a driver from New.
func (d *Driver) Close() error {
	var errs []error
	for _, c := range d.closers {
		errs = append(errs, c.Close())
	}
	d.closers = nil
	return errors.Join(errs...)
}

// route adds the target-actor header, which the router needs to find the guest. The signing
// connection adds the token for the same actor.
func route(ctx context.Context, k key) context.Context {
	return metadata.AppendToOutgoingContext(ctx, TargetActorHeader, k.String())
}

func ref(atespace, name string) *ateapiv1.ObjectRef {
	return &ateapiv1.ObjectRef{Atespace: atespace, Name: name}
}

// EnsureWorkspace implements execution.Compute. Substrate v1.0.1 gives an atespace no labels, so
// labels are dropped and the returned workspace has none.
func (d *Driver) EnsureWorkspace(ctx context.Context, name string, _ map[string]string) (execution.Workspace, error) {
	const op = "EnsureWorkspace"
	as, err := d.control.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: ref("", name)})
	if p := d.cfg.Provisioner; p != nil {
		// A caller without a role on a missing atespace gets PermissionDenied, not NotFound (the check runs
		// before the handler), and one whose policy is not written yet gets it too. Both go to the provisioner.
		if isCode(err, codeNotFound) || isCode(err, codes.PermissionDenied) {
			if perr := p.EnsureAtespace(ctx, name); perr != nil {
				return execution.Workspace{}, fail(op, execution.ErrUnavailable, "the provisioner could not ensure atespace %s: %v", name, perr)
			}
			as, err = d.control.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: ref("", name)})
		}
	} else if isCode(err, codeNotFound) {
		as, err = d.control.CreateAtespace(ctx, &ateapiv1.CreateAtespaceRequest{
			Atespace: &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: name}},
		})
		if isCode(err, codeAlreadyExists) {
			as, err = d.control.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: ref("", name)})
		}
	}
	if err != nil {
		return execution.Workspace{}, execution.GRPCError(op, err)
	}
	return execution.Workspace{Name: name, ID: as.GetMetadata().GetUid(), Active: as.GetMetadata().GetDeleteTime() == nil}, nil
}

// CreateSandbox implements execution.Compute. It refuses a spec that carries provider records,
// network rules or a filesystem policy. The production path never sends them: the adapter
// (execution.WithoutEgress, noegress.go) drops these fields, because the gateway owns egress and the
// guest launcher holds one fixed policy (decisions D1 and D2 of epic #1082). The refusal guards a
// caller that goes around the adapter.
func (d *Driver) CreateSandbox(ctx context.Context, workspace string, spec execution.SandboxSpec) (execution.Sandbox, error) {
	const op = "CreateSandbox"
	if err := checkSpec(op, spec); err != nil {
		return execution.Sandbox{}, err
	}
	if err := d.requireSigner(op); err != nil {
		return execution.Sandbox{}, err
	}
	if err := d.requireGatewayRoutes(op); err != nil {
		return execution.Sandbox{}, err
	}
	tmpl, err := renderTemplate(workspace, spec, d.cfg)
	if err != nil {
		return execution.Sandbox{}, fail(op, execution.ErrInvalid, "%v", err)
	}
	if _, err := d.control.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: ref("", workspace)}); err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	if _, err := d.control.CreateActorTemplate(ctx, &ateapiv1.CreateActorTemplateRequest{ActorTemplate: tmpl}); err != nil && !isCode(err, codeAlreadyExists) {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	// A name is claimed before CreateActor, and a used name gets no new actor (issue #1092). A live actor
	// of a claimed name is this create again: it is returned as an existing sandbox.
	actor, err := d.claimActorName(ctx, op, key{workspace, spec.Name})
	if err != nil {
		return execution.Sandbox{}, err
	}
	created := false
	if actor == nil {
		actor, err = d.control.CreateActor(ctx, &ateapiv1.CreateActorRequest{Actor: &ateapiv1.Actor{
			Metadata:      &ateapiv1.ResourceMetadata{Atespace: workspace, Name: spec.Name},
			ActorTemplate: ref(workspace, tmpl.GetMetadata().GetName()),
		}})
		created = err == nil
		if isCode(err, codeAlreadyExists) {
			// An actor that a driver made before the ledger had the name (an upgrade).
			actor, err = d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, spec.Name)})
		}
		if err != nil {
			return execution.Sandbox{}, execution.GRPCError(op, err)
		}
	}
	// Substrate creates an actor suspended (v1.0.0), and the Compute contract says a created sandbox
	// becomes ready: the driver resumes the actor it made (issue #1092). A sandbox that already
	// existed keeps its state; the reconciler stops and starts it.
	if created && actor.GetStatus().GetState() == ateapiv1.ActorState_ACTOR_STATE_SUSPENDED {
		return d.resume(ctx, op, workspace, spec.Name)
	}
	// An actor that already exists gets the policy too: it may come from a driver without issue #1444.
	if err := d.ensureEgressPolicy(ctx, op, key{workspace, spec.Name}); err != nil {
		return execution.Sandbox{}, err
	}
	return sandboxFrom(workspace, actor), nil
}

// SpecID implements [execution.SpecIdentifier]. Substrate objects have no labels, so the identity of
// a sandbox's spec is the name of its template: a hash of everything in the template. Sandbox.SpecID
// of an observed actor is the same name.
func (d *Driver) SpecID(workspace string, spec execution.SandboxSpec) (string, error) {
	if err := d.requireSigner("SpecID"); err != nil {
		return "", err
	}
	tmpl, err := renderTemplate(workspace, spec, d.cfg)
	if err != nil {
		return "", fail("SpecID", execution.ErrInvalid, "%v", err)
	}
	return tmpl.GetMetadata().GetName(), nil
}

var _ execution.SpecIdentifier = (*Driver)(nil)

// checkSpec refuses what the driver cannot carry out: provider records, network rules and filesystem
// paths. It is a guard against a caller that goes around the adapter (noegress.go), which drops these
// fields on purpose under decisions D1 and D2 of epic #1082 (egress belongs to the gateway, ADR 0097;
// the guest launcher applies one fixed policy, ADR 0119).
func checkSpec(op string, spec execution.SandboxSpec) error {
	if len(spec.Providers) > 0 || len(spec.Policy.Network) > 0 {
		return fail(op, execution.ErrUnsupported,
			"the Substrate driver has no provider records or network rules: egress belongs to the gateway (ADR 0097, ADR 0119)")
	}
	if fs := spec.Policy.Filesystem; fs.IncludeWorkdir || len(fs.ReadOnly) > 0 || len(fs.ReadWrite) > 0 {
		return fail(op, execution.ErrUnsupported,
			"the Substrate driver cannot carry a filesystem policy until the guest launcher config does (ADR 0045, ADR 0119, #1087)")
	}
	return nil
}

// GetSandbox implements execution.Compute.
func (d *Driver) GetSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		if isCode(err, codeNotFound) {
			d.forget(key{workspace, name})
		}
		return execution.Sandbox{}, execution.GRPCError("GetSandbox", err)
	}
	return sandboxFrom(workspace, actor), nil
}

// StartSandbox implements execution.Compute: it resumes the actor.
func (d *Driver) StartSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	return d.resume(ctx, "StartSandbox", workspace, name)
}

// StopSandbox implements execution.Compute: it suspends the actor, which keeps its volumes and its
// memory. Substrate has no stop that discards memory, so a stopped sandbox is observed as
// [execution.PhaseSuspended].
func (d *Driver) StopSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	return d.suspendWanted(ctx, "StopSandbox", workspace, name)
}

// DeleteSandbox implements execution.Compute. Substrate terminates a running actor's workload
// before it removes the record, so the driver does not suspend first (and takes no snapshot). The
// record may outlive the call in state DELETING: the outcome is then [execution.DeleteAccepted].
func (d *Driver) DeleteSandbox(ctx context.Context, workspace, name string) (execution.DeleteOutcome, error) {
	const op = "DeleteSandbox"
	k := key{workspace, name}
	g := d.pauseGate(k) // no guest traffic while the actor goes away
	g.cancelAll()       // nothing to keep: the actor's work ends with it
	paused := true
	unpause := func() {
		if paused {
			paused = false
			g.unpause()
		}
	}
	defer unpause()
	actor, err := d.control.DeleteActor(ctx, &ateapiv1.DeleteActorRequest{Actor: ref(workspace, name)})
	switch {
	case isCode(err, codeNotFound):
		unpause()
		d.forget(k)
		d.setOwed(k, false)
		d.forgetLineage(k)
		d.forgetAccess(k)
		return execution.DeleteAbsent, nil
	case err != nil:
		return "", execution.GRPCError(op, err)
	}
	// The actor is gone or going: drop the gate as soon as nothing uses it. A process that is
	// still registered drops it when it ends.
	unpause()
	d.forget(k)
	d.setOwed(k, false)
	d.forgetLineage(k)
	d.forgetAccess(k)
	if actor.GetStatus().GetState() == ateapiv1.ActorState_ACTOR_STATE_DELETING {
		return execution.DeleteAccepted, nil
	}
	return execution.DeleteCompleted, nil
}

// SuspendSandbox implements execution.Suspender. It first drains the driver's own router calls to
// the actor, because any request through the router would resume it again, then suspends it with
// synchronous durability, so that a return means the snapshot is saved, and verifies that it stayed
// suspended (see suspend).
func (d *Driver) SuspendSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	return d.suspendWanted(ctx, "SuspendSandbox", workspace, name)
}

// suspendWanted is a suspend that a caller asked for: after it, no snapshot owes the actor a resume.
func (d *Driver) suspendWanted(ctx context.Context, op, workspace, name string) (execution.Sandbox, error) {
	sb, _, err := d.suspend(ctx, op, workspace, name)
	if err == nil {
		d.setOwed(key{workspace, name}, false)
	}
	return sb, err
}

// ResumeSandbox implements execution.Suspender.
func (d *Driver) ResumeSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	return d.resume(ctx, "ResumeSandbox", workspace, name)
}

// suspendRetries is how many times suspend suspends an actor again that a late router request resumed.
const suspendRetries = 3

// suspend suspends an actor in three steps: drain, suspend, verify.
//
//  1. Drain: pause the gate, so no new router call starts; cancel the output streams; wait for
//     the short calls in flight to end (cancelled after DrainTimeout) and for every cancelled call
//     to return on the client side. Nothing waits for the guest to acknowledge.
//  2. SuspendActor, with synchronous durability.
//  3. Verify: read the actor. A request that left before the pause can still reach the router after
//     SuspendActor, and the router resumes the actor for it (ADR 0119, Verification). If the actor
//     is resuming or running, wait for it to settle and suspend it again, up to suspendRetries
//     times; then fail. suspend never reports a suspended actor that is not.
//
// sent reports whether a SuspendActor was sent: after it, a failed suspend can leave the actor
// suspended, and a caller that wants it running must resume it.
func (d *Driver) suspend(ctx context.Context, op, workspace, name string) (sb execution.Sandbox, sent bool, err error) {
	k := key{workspace, name}
	g := d.pauseGate(k)
	defer g.unpause()
	if err := g.drain(ctx, d.cfg.DrainTimeout); err != nil {
		return execution.Sandbox{}, false, fail(op, err, "drain the router calls to sandbox %s: %v", k, err)
	}
	for attempt := 0; ; attempt++ {
		sent = true // an error reply does not prove that the suspend did not happen
		if _, err := d.control.SuspendActor(ctx, &ateapiv1.SuspendActorRequest{
			Actor:   ref(workspace, name),
			Options: &ateapiv1.SuspendOptions{Mode: ateapiv1.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
		}); err != nil {
			return execution.Sandbox{}, sent, execution.GRPCError(op, err)
		}
		actor, err := d.settled(ctx, op, k)
		if err != nil {
			return execution.Sandbox{}, sent, err
		}
		switch actor.GetStatus().GetState() {
		case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED:
			sb := sandboxFrom(workspace, actor)
			sb.NewSnapshot = true
			return sb, sent, nil
		case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
		default:
			return execution.Sandbox{}, sent, fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s after suspend", k, phaseOf(actor))
		}
		if attempt == suspendRetries {
			d.resuspends.WithLabelValues(resuspendFailed).Inc()
			d.cfg.Logger.Warn("substrate: sandbox resumed again after every suspend; giving up",
				"op", op, "sandbox", k.String(), "suspends", attempt+1)
			return execution.Sandbox{}, sent, fail(op, execution.ErrUnavailable,
				"sandbox %s was resumed by a router request after each of %d suspends", k, attempt+1)
		}
		d.resuspends.WithLabelValues(resuspendRetried).Inc()
		d.cfg.Logger.Info("substrate: a late router request resumed the sandbox; suspending it again",
			"op", op, "sandbox", k.String(), "attempt", attempt+1)
	}
}

// settled reads the actor until it is not in a transition (resuming or suspending), bounded by
// DrainTimeout. It reads the control API only, which never resumes the actor. When the bound ends,
// during a read or between two, it fails with ErrUnavailable.
func (d *Driver) settled(ctx context.Context, op string, k key) (*ateapiv1.Actor, error) {
	return d.settledWithin(ctx, op, k, d.cfg.DrainTimeout)
}

// settledWithin is settled with another bound.
func (d *Driver) settledWithin(ctx context.Context, op string, k key, bound time.Duration) (*ateapiv1.Actor, error) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	for {
		actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(k.atespace, k.actor)})
		if err != nil {
			// The server can end the call on the propagated deadline a moment before the client's
			// ctx is done: that is the bound too.
			if ctx.Err() != nil || status.Code(err) == codes.DeadlineExceeded {
				return nil, fail(op, execution.ErrUnavailable, "sandbox %s did not settle after suspend: %v", k, err)
			}
			return nil, execution.GRPCError(op, err)
		}
		switch actor.GetStatus().GetState() {
		case ateapiv1.ActorState_ACTOR_STATE_RESUMING, ateapiv1.ActorState_ACTOR_STATE_SUSPENDING:
		default:
			return actor, nil
		}
		if err := sleep(ctx, d.cfg.PollInterval); err != nil {
			return nil, fail(op, execution.ErrUnavailable, "sandbox %s did not settle after suspend: %v", k, err)
		}
	}
}

// resume writes the actor's egress policy and then resumes the actor. When the policy write fails, the
// actor stays suspended: it does not run with a policy the driver did not write (issue #1444).
func (d *Driver) resume(ctx context.Context, op, workspace, name string) (execution.Sandbox, error) {
	if err := d.ensureEgressPolicy(ctx, op, key{workspace, name}); err != nil {
		return execution.Sandbox{}, err
	}
	k := key{workspace, name}
	// An actor with no snapshot boots for the first time. A revert of it would give an empty home, so
	// it gets one synchronous suspend as soon as it runs (issue #1481, decision 8).
	first := d.isSealOwed(k)
	if before, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)}); err != nil {
		// Without this read the driver cannot tell a first boot. The actor then runs without the seal, and a
		// revert of it would give an empty home: say so.
		d.cfg.Logger.Warn("substrate: could not read the actor to detect a first boot; no seal is taken", "op", op, "sandbox", k.String(), "err", err)
	} else {
		first = first || (before.GetStatus().GetState() == ateapiv1.ActorState_ACTOR_STATE_SUSPENDED && len(before.GetStatus().GetSnapshots()) == 0)
	}
	resp, err := d.retryResume(ctx, workspace, name)
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	d.setOwed(k, false)
	if first && resp.GetResumed() {
		if sb, ok := d.sealFirstBoot(ctx, op, k); ok {
			d.setSealOwed(k, false)
			return sb, nil
		}
	}
	return sandboxFrom(workspace, resp.GetActor()), nil
}

// retryResume sends ResumeActor, again with a growing wait while Substrate answers ResourceExhausted.
func (d *Driver) retryResume(ctx context.Context, workspace, name string) (resp *ateapiv1.ResumeActorResponse, err error) {
	err = d.retryExhausted(ctx, func() error {
		var rerr error
		resp, rerr = d.control.ResumeActor(ctx, &ateapiv1.ResumeActorRequest{Actor: ref(workspace, name)})
		return rerr
	})
	return resp, err
}

// sealFirstBoot waits for a first boot to finish, takes one synchronous suspend, and resumes the actor.
// It is best effort. ok is false when nothing changed (the boot did not finish in GuestReadyTimeout, the
// actor crashed, or the suspend failed): the caller then reports the actor as the first resume left it.
// When ok is true, sb is the state the actor really has. That is the resumed actor, or the suspended one
// when the second resume failed (it holds its snapshot, and the next resume starts it). An actor that is
// not sealed reverts to an empty home, so every failure here is logged.
func (d *Driver) sealFirstBoot(ctx context.Context, op string, k key) (sb execution.Sandbox, ok bool) {
	actor, err := d.settledWithin(ctx, op, k, d.cfg.GuestReadyTimeout)
	if err != nil || actor.GetStatus().GetState() != ateapiv1.ActorState_ACTOR_STATE_RUNNING {
		d.cfg.Logger.Warn("substrate: the first boot did not reach running; the actor has no snapshot", "op", op, "sandbox", k.String(), "err", err)
		return execution.Sandbox{}, false
	}
	sealed, _, err := d.suspend(ctx, op, k.atespace, k.actor)
	if err != nil {
		d.cfg.Logger.Warn("substrate: the suspend after the first boot failed; the actor has no snapshot", "op", op, "sandbox", k.String(), "err", err)
		return execution.Sandbox{}, false
	}
	resp, err := d.retryResume(ctx, k.atespace, k.actor)
	if err != nil {
		// The actor is suspended and holds its snapshot: report that, so the caller does not record a
		// running sandbox. The next resume starts it.
		d.cfg.Logger.Warn("substrate: the resume after the first suspend failed", "op", op, "sandbox", k.String(), "err", err)
		d.setOwed(k, true)
		return sealed, true
	}
	out := sandboxFrom(k.atespace, resp.GetActor())
	out.NewSnapshot = sealed.NewSnapshot
	return out, true
}

// requireRunning reads the actor from the control API, which never resumes it, and fails unless it
// is running. Exec and Upload call it first: a request to the router for a suspended actor would
// resume it, which is not what a caller of Exec on a stopped sandbox asked for.
func (d *Driver) requireRunning(ctx context.Context, op string, k key) error {
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(k.atespace, k.actor)})
	if err != nil {
		return execution.GRPCError(op, err)
	}
	if st := actor.GetStatus().GetState(); st != ateapiv1.ActorState_ACTOR_STATE_RUNNING {
		return fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not ready", k, phaseOf(actor))
	}
	if err := d.learn(ctx, k, actor); err != nil {
		return execution.GRPCError(op, err)
	}
	if uid := actor.GetMetadata().GetUid(); !d.isBound(k, uid) {
		return d.bind(ctx, op, k, uid)
	}
	return nil
}

// requireSigner fails closed without a guest API signing key: a sandbox could not be used, because
// its guest front refuses every unsigned call (issue #1309).
func (d *Driver) requireSigner(op string) error {
	if d.cfg.GuestSigner == nil {
		return fail(op, execution.ErrFailedPrecondition, "no guest API signing key (issue #1309)")
	}
	return nil
}

// fail builds an *execution.Error of a given kind.
func fail(op string, kind error, format string, args ...any) error {
	return &execution.Error{Op: op, Kind: kind, Err: fmt.Errorf(format, args...)}
}

// noteRouter records that the router connection is READY. signedConn calls it before and after every call.
func (d *Driver) noteRouter() {
	if d.routerState != nil && d.routerState() == connectivity.Ready {
		d.routerSeen.Store(true)
	}
}

// routerDown maps an Unavailable error of a router call that never reached a guest to
// [execution.ErrUnreachable]; it returns nil for any other error. A guest that does not listen yet answers through a
// working connection, which is why the callers retry Unavailable. A connection that has never been READY in
// this process and that the router or the driver refused on identity (a TLS alert, a client certificate the
// router refuses, a router certificate the driver does not trust) is in TRANSIENT_FAILURE and no retry fixes
// it, so the caller fails with the TLS error at once instead of waiting out the guest's start-up time
// (issue #1446). Connection refused and timeouts stay Unavailable, also in a new agentd process: a router
// outage that passes must not fail the runs that this process picks up (issue #1454). Once the connection has
// been READY, a failure is an outage (a router rollout, an eviction, a node drain): it stays Unavailable and
// the callers retry and resume.
//
// Known limit: the state check is a sample, not a watcher. A connection that goes READY and fails inside one
// call is still seen as a startup failure if the failure is an identity refusal.
func (d *Driver) routerDown(op string, err error) error {
	if status.Code(err) != codes.Unavailable || d.routerState == nil || d.routerRefusal == nil ||
		d.routerState() != connectivity.TransientFailure || d.routerSeen.Load() {
		return nil
	}
	refused := d.routerRefusal(err)
	if refused == nil {
		return nil
	}
	return &execution.Error{Op: op, Kind: execution.ErrUnreachable, Err: fmt.Errorf("TLS identity check with the atenet router failed: %w", refused)}
}
