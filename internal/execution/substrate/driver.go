// SPDX-License-Identifier: Apache-2.0

// Package substrate is the Agent Substrate driver of the execution seam (epic 1082, ADR 0119). It
// implements [execution.Compute], [execution.Suspender] and [execution.Snapshotter]; it has no
// [execution.Egress] half, because Substrate's atenet is default-deny and L7 rules and credentials
// belong to the Koinon gateway (ADR 0097).
//
// The mapping, with the version of Substrate it was written against (v1.0.0, protos in
// third_party/substrate):
//
//   - a workspace is an atespace (ADR 0002: an org is an atespace);
//   - a sandbox is an actor, created from an ActorTemplate that the driver derives from the
//     SandboxSpec and names by a hash of it, so equal specs share a template;
//   - StopSandbox and SuspendSandbox are one operation, SuspendActor: Substrate has no stopped
//     state that is not a suspended one. Both report [execution.PhaseSuspended];
//   - Exec has no control-API call (upstream issue 185), so it runs over the in-guest
//     ProcessService, and Upload over FileSystemService, both through the atenet router with the
//     header ate-target-actor;
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
	"github.com/stateracloud/koinon/internal/harness"
)

// TargetActorHeader selects the actor a request to the atenet router goes to, as
// "<atespace>/<actor>" (Substrate env client, internal/ate TargetActorHeader).
const TargetActorHeader = "ate-target-actor"

// Config tunes the driver. The zero value is usable: every field has a default.
type Config struct {
	// PollInterval is how long the driver waits between reads of an actor's state while it waits
	// for it to run. Default 250 ms; the wait backs off to eight times that.
	PollInterval time.Duration
	// StreamStall is how long an output stream may fail again and again without delivering a byte
	// before the process fails with [execution.ErrUnavailable]. Default 2 minutes.
	StreamStall time.Duration
	// GuestReadyTimeout is how long Exec and Upload retry a guest that is not reachable yet. An
	// actor is running before its guest daemon listens. Default 30 s.
	GuestReadyTimeout time.Duration
	// SnapshotStorageLocation is the ActorTemplate's snapshot storage location, as Substrate's
	// install names it. Empty leaves the template's field unset, which Substrate's default fills.
	SnapshotStorageLocation string
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
	files   ateenvv1alpha.FileSystemServiceClient
	cfg     Config
	closers []io.Closer

	mu    sync.Mutex
	gates map[key]*gate
	// owed holds the actors that SnapshotSandbox suspended and could not resume. A retry of the
	// snapshot resumes them, because the seam says the sandbox keeps running.
	owed map[key]bool
	// tiers caches the tier of a template by atespace and name. A template name is a hash of its
	// content, so the entry never goes stale.
	tiers map[key]harness.Tier

	resuspends *prometheus.CounterVec
}

// The driver implements the compute half and both optional capabilities. It has no Egress half.
var (
	_ execution.Compute     = (*Driver)(nil)
	_ execution.Suspender   = (*Driver)(nil)
	_ execution.Snapshotter = (*Driver)(nil)
)

// key names an actor.
type key struct{ atespace, actor string }

func (k key) String() string { return k.atespace + "/" + k.actor }

// New returns a driver on existing connections: control reaches ateapi, router reaches the atenet
// router. The caller owns both.
func New(control, router grpc.ClientConnInterface, cfg Config) *Driver {
	return &Driver{
		control:    ateapiv1.NewControlClient(control),
		process:    ateenvv1alpha.NewProcessServiceClient(router),
		files:      ateenvv1alpha.NewFileSystemServiceClient(router),
		cfg:        cfg.withDefaults(),
		gates:      map[key]*gate{},
		owed:       map[key]bool{},
		tiers:      map[key]harness.Tier{},
		resuspends: newResuspends(),
	}
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

// route adds the target-actor header, which the router needs to find the guest.
func route(ctx context.Context, k key) context.Context {
	return metadata.AppendToOutgoingContext(ctx, TargetActorHeader, k.String())
}

func ref(atespace, name string) *ateapiv1.ObjectRef {
	return &ateapiv1.ObjectRef{Atespace: atespace, Name: name}
}

// EnsureWorkspace implements execution.Compute. Substrate v1.0.0 gives an atespace no labels, so
// labels are dropped and the returned workspace has none.
func (d *Driver) EnsureWorkspace(ctx context.Context, name string, _ map[string]string) (execution.Workspace, error) {
	const op = "EnsureWorkspace"
	as, err := d.control.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: ref("", name)})
	if isCode(err, codeNotFound) {
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

// CreateSandbox implements execution.Compute. It refuses a spec that asks for what Substrate
// cannot do inside the driver: provider records and network rules belong to the gateway (ADR 0097),
// so a driver that dropped them would run a sandbox with less fencing than the caller asked for.
func (d *Driver) CreateSandbox(ctx context.Context, workspace string, spec execution.SandboxSpec) (execution.Sandbox, error) {
	const op = "CreateSandbox"
	if err := checkSpec(op, spec); err != nil {
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
	actor, err := d.control.CreateActor(ctx, &ateapiv1.CreateActorRequest{Actor: &ateapiv1.Actor{
		Metadata:      &ateapiv1.ResourceMetadata{Atespace: workspace, Name: spec.Name},
		ActorTemplate: ref(workspace, tmpl.GetMetadata().GetName()),
	}})
	if isCode(err, codeAlreadyExists) {
		actor, err = d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, spec.Name)})
	}
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	return sandboxFrom(workspace, actor), nil
}

// checkSpec refuses what the driver cannot carry out. Provider records and network rules belong to
// the gateway (ADR 0097). Filesystem paths belong to the guest launcher's Landlock configuration
// (ADR 0119, #1087), which this driver does not render yet: a driver that dropped them would run
// the sandbox without the fencing the caller asked for, and ADR 0045 makes Landlock a hard
// requirement.
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
		return execution.DeleteAbsent, nil
	case err != nil:
		return "", execution.GRPCError(op, err)
	}
	// The actor is gone or going: drop the gate as soon as nothing uses it. A process that is
	// still registered drops it when it ends.
	unpause()
	d.forget(k)
	d.setOwed(k, false)
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
			return sandboxFrom(workspace, actor), sent, nil
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
	ctx, cancel := context.WithTimeout(ctx, d.cfg.DrainTimeout)
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

func (d *Driver) resume(ctx context.Context, op, workspace, name string) (execution.Sandbox, error) {
	resp, err := d.control.ResumeActor(ctx, &ateapiv1.ResumeActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	d.setOwed(key{workspace, name}, false)
	return sandboxFrom(workspace, resp.GetActor()), nil
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
	return nil
}

// fail builds an *execution.Error of a given kind.
func fail(op string, kind error, format string, args ...any) error {
	return &execution.Error{Op: op, Kind: kind, Err: fmt.Errorf(format, args...)}
}
