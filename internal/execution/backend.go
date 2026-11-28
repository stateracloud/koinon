// SPDX-License-Identifier: Apache-2.0

// Package execution is Koinon's contract with the execution layer (ADR 0016): workspaces,
// provider records, policy, sandbox lifecycle and exec streams. The seam has two halves:
// [Compute] (sandbox lifecycle, exec, upload) and [Egress] (provider records and L7 policy, which
// ADR 0097 moves to the Koinon gateway). The OpenShell gateway is the only driver today (package
// execution/openshell); the types here carry no OpenShell types so that upstream churn stays inside
// the implementation. Package executiontest holds the contract every driver must pass.
//
// The types are deliberately narrower than the OpenShell API. They cannot express the
// settings ADR 0045 and ADR 0048 forbid: there is no audit enforcement, no TLS skip, no
// uninspected credentials, no sandbox environment variables, Landlock is always a hard
// requirement and every provider credential gets a stable placeholder.
package execution

import (
	"context"
	"io"
	"time"
)

// Compute runs sandboxes: it creates, starts, stops and deletes them, runs commands in them and
// uploads files into them. It carries nothing about the network or about credentials, so a driver
// that has no such notion (Agent Substrate, epic 1082) implements it as it is. The contract suite
// in package executiontest says what every driver must do. "Ensure" methods are idempotent: they
// create what is missing and do nothing when the record already matches.
//
// A driver may also implement the optional capabilities [Suspender] and [Snapshotter].
type Compute interface {
	// EnsureWorkspace makes sure the org's workspace exists (one per org, ADR 0002). Labels
	// apply only on creation; OpenShell 0.1.7 has no workspace update (its UpdateSandbox, off by
	// default, changes a stopped sandbox, not a workspace).
	EnsureWorkspace(ctx context.Context, name string, labels map[string]string) (Workspace, error)

	// CreateSandbox creates a sandbox, or returns the existing one with the same name.
	// It does not compare an existing sandbox with spec; the caller decides on drift.
	CreateSandbox(ctx context.Context, workspace string, spec SandboxSpec) (Sandbox, error)

	// GetSandbox returns a sandbox, or an error matching ErrNotFound.
	GetSandbox(ctx context.Context, workspace, name string) (Sandbox, error)

	// StartSandbox starts a stopped sandbox. Starting a running one is a no-op.
	StartSandbox(ctx context.Context, workspace, name string) (Sandbox, error)

	// StopSandbox stops a sandbox and keeps its home volume. Stopping a stopped one is a no-op. A
	// stopped sandbox is observed as PhaseStopped, or as PhaseSuspended on a driver whose only
	// stopped state is a suspended one (Agent Substrate).
	StopSandbox(ctx context.Context, workspace, name string) (Sandbox, error)

	// DeleteSandbox requests the deletion of a sandbox. A missing sandbox is not an error
	// (DeleteAbsent). Deletion may finish asynchronously (DeleteAccepted): the caller does not
	// wait for it but observes it, because GetSandbox reports PhaseDeleting until it is done.
	DeleteSandbox(ctx context.Context, workspace, name string) (DeleteOutcome, error)

	// Exec starts a command in a ready sandbox without a TTY: stdin and stdout are byte
	// streams (no echo, no CRLF), suitable for newline-delimited JSON-RPC such as ACP.
	// Cancelling ctx, or calling Process.Close, closes the streams and ends the command.
	// Exec on a sandbox that is not ready fails with ErrFailedPrecondition.
	Exec(ctx context.Context, workspace, sandbox string, req ExecRequest) (Process, error)

	// Upload extracts the tar stream into destDir in a ready sandbox, creating destDir first,
	// as the sandbox user. It uses the execution layer's flow-controlled upload channel, which
	// Exec stdin is not: a large stdin stream can outrun the sandbox and be killed (ADR 0032,
	// issue 169). The archive is extracted by a fixed command, so the caller shapes it: no "./"
	// root entry, nothing that must not be extracted. Extraction is not atomic: after an error
	// destDir may hold part of the archive. A failure of the stream reading tar aborts the
	// upload without end-of-file and is returned in the error chain. An execution layer that
	// does not support uploads fails with ErrUnsupported.
	Upload(ctx context.Context, workspace, sandbox, destDir string, tar io.Reader) error
}

// Egress is the network and credential half of the seam: provider records, the credentials in
// them and the L7 policy of a sandbox. It exists only because OpenShell enforces egress inside the
// sandbox. ADR 0097 moves L7 rules and credential injection to the Koinon gateway, which holds
// per-route rules (epic 1082, D2), so a driver behind the gateway does not implement Egress and no
// new caller may depend on it. The OpenShell driver keeps it until it is removed (epic 1082, D9).
// Code that needs no egress takes [Compute], not [Backend].
type Egress interface {
	// EnsureProvider makes sure the provider's workspace-scoped profile and the provider
	// record exist and match. Credential values are write-only: they are pushed when the
	// record is created and whenever CredentialRevision changes.
	EnsureProvider(ctx context.Context, workspace string, p Provider) error

	// DeleteProvider removes a provider record. A missing record is not an error.
	DeleteProvider(ctx context.Context, workspace, name string) error

	// DeleteProviderProfile removes a workspace-scoped provider profile. A missing profile is
	// not an error. OpenShell refuses while a provider attached to a sandbox uses it.
	DeleteProviderProfile(ctx context.Context, workspace, id string) error

	// EnsurePolicy makes the sandbox's policy match p and returns the policy version. On a
	// running sandbox only the network rules may change (OpenShell rejects other changes);
	// those need a new sandbox.
	EnsurePolicy(ctx context.Context, workspace, sandbox string, p Policy) (uint32, error)
}

// Backend drives the execution layer as OpenShell shapes it: [Compute] and [Egress] together.
// The reconciler takes a Backend while OpenShell is the only driver. It is the one place that
// joins the halves, so removing Egress later changes this line and its callers, not the drivers.
type Backend interface {
	Compute
	Egress
}

// Suspender is an optional capability of a driver: suspend a sandbox keeping its memory, and
// resume it. A sandbox that is suspended is observed as PhaseSuspended. The OpenShell driver
// does not implement it (StopSandbox keeps the home volume only).
type Suspender interface {
	// SuspendSandbox suspends a running sandbox. Suspending a suspended one is a no-op.
	SuspendSandbox(ctx context.Context, workspace, name string) (Sandbox, error)
	// ResumeSandbox resumes a suspended sandbox. Resuming a running one is a no-op.
	ResumeSandbox(ctx context.Context, workspace, name string) (Sandbox, error)
}

// Snapshotter is an optional capability of a driver: save the state of a sandbox and start a new
// sandbox from it (epic 1082, D5). The OpenShell driver does not implement it. A snapshot belongs
// to the workspace it was taken in (ADR 0002): restoring it in another workspace fails with
// ErrNotFound. Snapshot.Kind says what the driver saved; D5 wants SnapshotDiskMemory, which a
// driver that can save memory must use, and the contract suite accepts both kinds.
type Snapshotter interface {
	// SnapshotSandbox saves the state of a sandbox. The sandbox keeps running.
	SnapshotSandbox(ctx context.Context, workspace, name string) (Snapshot, error)
	// RestoreSandbox creates the sandbox spec.Name from a snapshot. It fails with ErrNotFound
	// for an unknown snapshot or one from another workspace, with ErrInvalid when the digest of
	// snapshot does not match the saved one or its content, and with ErrAlreadyExists when a
	// sandbox with the name of spec exists.
	RestoreSandbox(ctx context.Context, workspace string, snapshot SnapshotRef, spec SandboxSpec) (Sandbox, error)
}

// GatewayAccess is what a sandbox may reach at the Koinon gateway in addition to the model and MCP
// listeners, which every sandbox reaches (issue #1444). It is not part of [SandboxSpec]: a change of
// access never replaces a sandbox.
type GatewayAccess struct {
	// CodeHostEgress lets the sandbox reach the gateway's egress listener. The reconciler sets it
	// only for an agent with a code-host grant, on an install with an egress listener: the same
	// condition as the OpenShell policy entry of that listener.
	CodeHostEgress bool
}

// GatewayAccessSetter is an optional capability of a driver that keeps a per-sandbox allow list to
// the gateway (the actor egress policy of Agent Substrate). The OpenShell driver does not implement
// it: its policy entries carry the same rule.
type GatewayAccessSetter interface {
	// SetGatewayAccess records the access of sandbox name and applies it at once to a sandbox that
	// exists, in place (in the control plane; the data plane can cache the old policy for a short
	// time). A sandbox that does not exist yet gets it when it is created or restored. A
	// driver that has no access recorded for a sandbox gives it the zero value: it fails closed.
	SetGatewayAccess(ctx context.Context, workspace, name string, access GatewayAccess) error
	// ForgetGatewayAccess drops the access recorded for sandbox name, with no call to the control plane.
	// The reconciler calls it for a name that no sandbox uses again (an old Substrate actor name, issue
	// #1386), so that the record does not outlive the name.
	ForgetGatewayAccess(workspace, name string)
}

// GatewayAccessSetterOf returns the capability of b, or nil when b has none. It looks through the
// adapter of [WithoutEgress].
func GatewayAccessSetterOf(b any) GatewayAccessSetter {
	if n, ok := b.(noEgress); ok {
		b = n.Compute
	}
	s, _ := b.(GatewayAccessSetter)
	return s
}

// ActorNamer is an optional capability of a driver whose sandbox runs as an actor with its own identity (Agent
// Substrate, issue #1386). Substrate signs a JWT for the actor, and the gateway maps the actor name to an agent run,
// so a name must never come back for another run: a new actor gets a new name, never a reused one.
type ActorNamer interface {
	// NewActorName returns a new, unique name for the actor of sandbox. It is "<sandbox>-<8 hex>" on Substrate.
	// The driver does not shorten a name that is too long for its backend; the create then fails with ErrInvalid.
	NewActorName(sandbox string) string
}

// ActorNamerOf returns the [ActorNamer] of b, or false when b has none (the OpenShell driver has none). It looks
// through the adapter of [WithoutEgress] and any wrapper with an Unwrap method that returns a [Compute].
func ActorNamerOf(b Backend) (ActorNamer, bool) {
	var cur any = b
	for range 8 { // bounded: a wrapper chain is short, and a cycle must not hang
		if n, ok := cur.(ActorNamer); ok {
			return n, true
		}
		u, ok := cur.(interface{ Unwrap() Compute })
		if !ok {
			return nil, false
		}
		cur = u.Unwrap()
	}
	return nil, false
}

// Reverter is an optional capability of a driver: revert a crashed or dead sandbox to its last snapshot
// and check that its guest front answers (issue #1481). The OpenShell driver does not implement it.
type Reverter interface {
	// RevertSandbox reverts a sandbox that is CRASHED, or RUNNING with a dead front, to its last
	// snapshot and resumes it. RevertActor accepts both states (Substrate cmd/ateapi/internal/controlapi/
	// workflow_revert.go, revertableStates). It fails with ErrNoSnapshot when the sandbox never
	// suspended: a revert would give an empty home. It fails with ErrRevertResume for every failure
	// after the revert worked (the egress policy write and the resume): the sandbox then holds its
	// snapshot, and the caller must not delete it.
	RevertSandbox(ctx context.Context, workspace, name string) (Sandbox, error)
	// CheckFront asks the guest front of a running sandbox to answer. It returns nil when the front
	// answers and ErrFrontDown when it does not. A slow answer of the control API in the same call is
	// no verdict on the front and is returned as another error. It never resumes the sandbox.
	CheckFront(ctx context.Context, workspace, name string) error
}

// ReverterOf returns the [Reverter] of b, or false when b has none. It looks through wrappers like
// [ActorNamerOf].
func ReverterOf(b Backend) (Reverter, bool) {
	var cur any = b
	for range 8 {
		if n, ok := cur.(Reverter); ok {
			return n, true
		}
		u, ok := cur.(interface{ Unwrap() Compute })
		if !ok {
			return nil, false
		}
		cur = u.Unwrap()
	}
	return nil, false
}

// SnapshotKind says what a snapshot holds.
type SnapshotKind string

// Snapshot kinds.
const (
	// SnapshotDisk holds the writable disk only; the restored sandbox boots.
	SnapshotDisk SnapshotKind = "disk"
	// SnapshotDiskMemory holds disk and memory; the restored sandbox continues where it was.
	SnapshotDiskMemory SnapshotKind = "disk+memory"
)

// SnapshotRef names a snapshot and pins its content. The digest lets the caller check that the
// bytes it restores are the bytes it saved.
type SnapshotRef struct {
	ID string
	// Digest is "<algorithm>:<hex>", for example "sha256:...". It is never empty.
	Digest string
}

// Snapshot is a saved sandbox state.
type Snapshot struct {
	SnapshotRef
	Kind      SnapshotKind
	Workspace string
	// Sandbox is the name of the sandbox the snapshot was taken from.
	Sandbox   string
	CreatedAt time.Time
}

// Workspace is an OpenShell workspace.
type Workspace struct {
	Name   string
	ID     string
	Labels map[string]string
	Active bool
}

// Provider is a provider record plus the workspace-scoped profile it is created from.
type Provider struct {
	// Name of the provider record; sandboxes attach it by this name.
	Name string
	// Profile describes the credential and the endpoints it may be sent to.
	Profile ProviderProfile
	// Credentials maps each credential's environment variable to its value. Values never
	// leave the gateway; the sandbox sees placeholders.
	Credentials map[string]string
	// CredentialRevision identifies the credential values (for example the Secret's
	// resourceVersion). Values are pushed when it changes; with an empty revision they are
	// pushed only when the record is created. It must not be derived from the values.
	CredentialRevision string
	// Config is non-secret provider configuration.
	Config map[string]string
	// Labels are applied to the provider record.
	Labels map[string]string
}

// ProviderProfile is the type of a provider: which credentials it holds and where they may
// be sent. Every credential gets a stable placeholder (ADR 0048), so a rotated value
// reaches running processes without a restart.
type ProviderProfile struct {
	ID          string
	DisplayName string
	Description string
	Category    ProfileCategory
	Credentials []ProfileCredential
	Endpoints   []Endpoint
	// Binaries are the absolute paths of the executables allowed to use the endpoints.
	Binaries []string
}

// ProfileCategory classifies a provider profile.
type ProfileCategory string

// Profile categories.
const (
	CategoryOther         ProfileCategory = "other"
	CategoryInference     ProfileCategory = "inference"
	CategorySourceControl ProfileCategory = "source_control"
)

// ProfileCredential is one credential of a provider profile.
type ProfileCredential struct {
	Name string
	// EnvVars are the environment variables that carry the credential's placeholder.
	EnvVars  []string
	Required bool
	// AuthStyle is how the credential is injected, for example "bearer".
	AuthStyle string
	// HeaderName is the header that carries the credential, for example "authorization".
	HeaderName string
}

// Policy is a sandbox policy. Network access is denied unless a rule allows it.
type Policy struct {
	Filesystem FilesystemPolicy
	// Network rules keyed by name.
	Network map[string]NetworkRule
}

// FilesystemPolicy lists the paths a sandboxed process may read and write.
type FilesystemPolicy struct {
	IncludeWorkdir bool
	ReadOnly       []string
	ReadWrite      []string
}

// NetworkRule allows a set of binaries to reach a set of endpoints.
type NetworkRule struct {
	Endpoints []Endpoint
	// Binaries are absolute executable paths.
	Binaries []string
}

// Endpoint is one network destination. Endpoints with a Protocol are inspected at L7 and
// always enforced.
type Endpoint struct {
	Host string
	Port uint32
	// Protocol is the L7 protocol ("rest", "mcp"), or empty for an L4-only endpoint.
	Protocol string
	// Path restricts the endpoint to a path prefix pattern, for example "/api/v1/**".
	Path string
	// Access is a preset for L7 endpoints. Allow adds rules on top.
	Access Access
	Allow  []L7Rule
	Deny   []L7Rule
}

// Access is an L7 access preset.
type Access string

// Access presets.
const (
	AccessNone      Access = ""
	AccessReadOnly  Access = "read_only"
	AccessReadWrite Access = "read_write"
	AccessFull      Access = "full"
)

// L7Rule matches requests by method, path pattern and, for REST, query parameters.
type L7Rule struct {
	Method string
	Path   string
	// Query maps a query parameter name to a glob. In an allow rule every value of the
	// parameter must match; in a deny rule the parameter must be present and one matching
	// value is enough (OpenShell policy schema, unchanged from 0.1.5 to 0.1.7).
	Query map[string]string
}

// SandboxSpec describes a sandbox to create. There is intentionally no environment field:
// configuration comes from the image and credentials from provider records (ADR 0045).
type SandboxSpec struct {
	Name  string
	Image string
	// RuntimeClassName is empty for the gateway default (kata, ADR 0045).
	RuntimeClassName string
	// Command is the sandbox's main process. Empty uses the gateway's default.
	Command   []string
	Providers []string
	Policy    Policy
	Labels    map[string]string
	// The agent container's CPU and memory requests and limits, as Kubernetes quantities such as
	// "250m" and "2Gi" (ADR 0083). An empty value is left out. OpenShell mirrors a limit into the
	// request when the request is absent, so a spec that gives limits only reserves its limits.
	CPURequest, CPULimit       string
	MemoryRequest, MemoryLimit string
}

// Sandbox is the observed state of a sandbox.
type Sandbox struct {
	Name      string
	ID        string
	Workspace string
	Labels    map[string]string
	// SpecID names the spec the sandbox was made from, for a backend that keeps no labels (see
	// [SpecIdentifier]). Empty for a backend that does.
	SpecID        string
	Phase         Phase
	PolicyVersion uint32
	// Enforcement is the network enforcement mode of the sandbox's policy as the gateway reports it
	// (ADR 0045): see the Enforcement constants. Empty when the backend does not report a policy.
	Enforcement string
	// ExitCode is the main process's last exit code, if it has exited.
	ExitCode *int32
	// Reason and Message describe the latest condition that is not true, if any.
	Reason  string
	Message string
	// NewSnapshot is true only on the sandbox that a suspend (or the first-boot seal) returns: the call
	// took a new snapshot. The Substrate API gives no snapshot time, and the driver gives none either: the
	// reconciler records the database clock that it read before the call (one clock for every time the
	// crash path compares, issue #1481).
	NewSnapshot bool
}

// SpecIdentifier is implemented by a backend whose sandboxes carry no labels (Agent Substrate has
// none: its objects have a name and no label map). The reconciler cannot compare a spec hash label
// with such a backend, so it asks for the identity the backend derives from a spec and compares it
// with [Sandbox.SpecID] of the observed sandbox. The identity must live as long as the sandbox and
// change with every field that cannot change on an existing sandbox (image, runtime class, resources).
type SpecIdentifier interface {
	SpecID(workspace string, spec SandboxSpec) (string, error)
}

// Phase is a sandbox lifecycle phase.
type Phase string

// Sandbox phases.
const (
	PhaseUnknown      Phase = "Unknown"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseError        Phase = "Error"
	PhaseDeleting     Phase = "Deleting"
	PhaseStopping     Phase = "Stopping"
	PhaseStopped      Phase = "Stopped"
	PhaseStarting     Phase = "Starting"
	PhaseCompleted    Phase = "Completed"
	PhaseSuspended    Phase = "Suspended"
)

// IsStopped reports whether the phase is a stopped one that keeps the sandbox state: PhaseStopped,
// or PhaseSuspended on a driver whose only stopped state is a suspended one (see StopSandbox).
func (p Phase) IsStopped() bool { return p == PhaseStopped || p == PhaseSuspended }

// ExecRequest is a command to run in a sandbox.
type ExecRequest struct {
	// Command is the executable and its arguments; it is not parsed by a shell.
	Command []string
	Workdir string
	// Timeout ends the command after the given duration. Zero means no timeout.
	Timeout time.Duration
	// LoginShell runs the command through a login shell (bash -lc), which sources the
	// sandbox user's profile files. Off by default for predictable startup.
	LoginShell bool
	// Stderr receives the command's standard error. Nil discards it. It is written from the
	// stream's receiving goroutine, so a blocking writer stalls stdout too.
	Stderr io.Writer
}

// Process is a running command. Stdin, Stdout and Wait may be used from different
// goroutines.
type Process interface {
	// Stdin is the command's standard input. Close sends end-of-file.
	Stdin() io.WriteCloser
	// Stdout is the command's standard output. It returns io.EOF after the command exits
	// and its output is drained, or the stream's error if the stream fails. Output is not
	// buffered: read it continuously, or the stream (and Wait) stalls.
	Stdout() io.Reader
	// Wait blocks until the command exits and returns its exit code. It returns an error
	// if the stream failed or ended without an exit status.
	Wait() (int, error)
	// Close ends the stream, which ends the command. It is safe to call more than once.
	Close() error
}

// DeleteOutcome says what a delete request did.
type DeleteOutcome string

const (
	// DeleteCompleted: the sandbox is gone from the gateway.
	DeleteCompleted DeleteOutcome = "completed"
	// DeleteAccepted: the delete is under way and the sandbox record still exists; observe it with
	// GetSandbox until it is gone.
	DeleteAccepted DeleteOutcome = "accepted"
	// DeleteAbsent: there was no such sandbox.
	DeleteAbsent DeleteOutcome = "absent"
)

// The network enforcement modes a sandbox's policy can be in (ADR 0045, ADR 0075). Koinon renders
// every inspected endpoint as enforced, and the evidence of a run checks that the sandbox still says so.
const (
	// EnforcementEnforce: every inspected (L7) endpoint enforces (L4-only endpoints have no mode).
	EnforcementEnforce = "enforce"
	// EnforcementAudit: some inspected endpoint only audits. ADR 0045 forbids it.
	EnforcementAudit = "audit"
	// EnforcementUnknown: the gateway reported no policy.
	EnforcementUnknown = "unknown"
)
