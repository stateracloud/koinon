// SPDX-License-Identifier: Apache-2.0

// Package execution is Koinon's contract with the execution layer (ADR 0016): workspaces,
// provider records, policy, sandbox lifecycle and exec streams. The OpenShell gateway is the
// implementation (package execution/openshell); the types here carry no OpenShell types so
// that upstream churn stays inside the implementation.
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

// Backend drives the execution layer. "Ensure" methods are idempotent: they create what is
// missing, update what differs and do nothing when the record already matches.
type Backend interface {
	// EnsureWorkspace makes sure the org's workspace exists (one per org, ADR 0002). Labels
	// apply only on creation; OpenShell 0.1.7 has no workspace update (its UpdateSandbox, off by
	// default, changes a stopped sandbox, not a workspace).
	EnsureWorkspace(ctx context.Context, name string, labels map[string]string) (Workspace, error)

	// EnsureProvider makes sure the provider's workspace-scoped profile and the provider
	// record exist and match. Credential values are write-only: they are pushed when the
	// record is created and whenever CredentialRevision changes.
	EnsureProvider(ctx context.Context, workspace string, p Provider) error

	// DeleteProvider removes a provider record. A missing record is not an error.
	DeleteProvider(ctx context.Context, workspace, name string) error

	// DeleteProviderProfile removes a workspace-scoped provider profile. A missing profile is
	// not an error. OpenShell refuses while a provider attached to a sandbox uses it.
	DeleteProviderProfile(ctx context.Context, workspace, id string) error

	// CreateSandbox creates a sandbox, or returns the existing one with the same name.
	// It does not compare an existing sandbox with spec; the caller decides on drift.
	CreateSandbox(ctx context.Context, workspace string, spec SandboxSpec) (Sandbox, error)

	// GetSandbox returns a sandbox, or an error matching ErrNotFound.
	GetSandbox(ctx context.Context, workspace, name string) (Sandbox, error)

	// StartSandbox starts a stopped sandbox. Starting a running one is a no-op.
	StartSandbox(ctx context.Context, workspace, name string) (Sandbox, error)

	// StopSandbox stops a sandbox and keeps its home volume. Stopping a stopped one is a no-op.
	StopSandbox(ctx context.Context, workspace, name string) (Sandbox, error)

	// DeleteSandbox requests the deletion of a sandbox. A missing sandbox is not an error
	// (DeleteAbsent). Deletion may finish asynchronously (DeleteAccepted): the caller does not
	// wait for it but observes it, because GetSandbox reports PhaseDeleting until it is done.
	DeleteSandbox(ctx context.Context, workspace, name string) (DeleteOutcome, error)

	// EnsurePolicy makes the sandbox's policy match p and returns the policy version. On a
	// running sandbox only the network rules may change (OpenShell rejects other changes);
	// those need a new sandbox.
	EnsurePolicy(ctx context.Context, workspace, sandbox string, p Policy) (uint32, error)

	// Exec starts a command in a ready sandbox without a TTY: stdin and stdout are byte
	// streams (no echo, no CRLF), suitable for newline-delimited JSON-RPC such as ACP.
	// Cancelling ctx, or calling Process.Close, closes the streams and ends the command.
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
	// ClaimLegacy lets EnsureProvider claim a record made before the install label existed
	// (issue 461): labels are immutable, so it deletes the record and creates it again with the
	// label. Set it only while no sandbox attaches the record (OpenShell may refuse to delete an
	// attached provider, and a delete would strip a live sandbox of it). Without it, an unlabeled
	// record is taken as this install's and updated in place.
	ClaimLegacy bool
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
	Name          string
	ID            string
	Workspace     string
	Labels        map[string]string
	Phase         Phase
	PolicyVersion uint32
	// ExitCode is the main process's last exit code, if it has exited.
	ExitCode *int32
	// Reason and Message describe the latest condition that is not true, if any.
	Reason  string
	Message string
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
)

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
