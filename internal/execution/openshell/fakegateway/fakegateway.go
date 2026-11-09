// SPDX-License-Identifier: Apache-2.0

// Package fakegateway is an in-process OpenShell gateway for tests. It implements the
// parts of openshell.v1.OpenShell that Koinon's execution backend uses, with the gateway
// semantics Koinon depends on (verified against OpenShell v0.1.7's server code):
//
//   - workspaces, sandboxes, providers and profiles report NotFound and AlreadyExists;
//   - provider updates merge credentials and config (an empty value removes a key) and
//     keep the type and metadata immutable;
//   - profile updates check the expected resource version (Aborted on a mismatch);
//   - a sandbox-scoped policy update may change network rules only, and an update equal
//     to the latest revision keeps its version;
//   - starting a sandbox that is not stopped and stopping one that is not ready fail with
//     FailedPrecondition (the real gateway is wider: it also starts a Completed or Suspended
//     sandbox, accepts a stop in more phases and treats a stop of a Stopped sandbox as settled,
//     none of which this fake models);
//   - ExecSandboxInteractive validates the start message, sends headers once the command
//     runs, streams stdin until the client half-closes and ends with an exit event.
//
// Exec runs registered Go functions in place of programs.
package fakegateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"regexp"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// Command stands in for a program run by exec. It reads stdin until EOF or until it has
// what it needs, and returns the exit code. ctx ends when the client cancels the stream.
type Command func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int

// Cat copies stdin to stdout, like cat(1).
func Cat(_ context.Context, _ []string, stdin io.Reader, stdout, _ io.Writer) int {
	if _, err := io.Copy(stdout, stdin); err != nil {
		return 1
	}
	return 0
}

type key struct{ workspace, name string }

// Gateway is a fake OpenShell gateway. The zero value is not usable; call New.
type Gateway struct {
	openshellv1.UnimplementedOpenShellServer

	// NoStablePlaceholder makes the gateway behave like one that predates
	// stable_placeholder: lint reports no support and imports drop the field.
	NoStablePlaceholder bool

	mu         sync.Mutex
	seq        int
	workspaces map[string]*datamodelv1.Workspace
	profiles   map[key]*openshellv1.ProviderProfile
	providers  map[key]*datamodelv1.Provider
	sandboxes  map[key]*openshellv1.Sandbox
	policies   map[key][]*sandboxv1.SandboxPolicy
	commands   map[string]Command
	execs      []*openshellv1.ExecSandboxRequest
	calls      map[string]int
	faults     map[string][]error

	ssh *sshState

	srv *grpc.Server
	lis *bufconn.Listener
}

// New returns an empty gateway.
func New() *Gateway {
	return &Gateway{
		workspaces: map[string]*datamodelv1.Workspace{},
		profiles:   map[key]*openshellv1.ProviderProfile{},
		providers:  map[key]*datamodelv1.Provider{},
		sandboxes:  map[key]*openshellv1.Sandbox{},
		policies:   map[key][]*sandboxv1.SandboxPolicy{},
		commands:   map[string]Command{},
		calls:      map[string]int{},
		faults:     map[string][]error{},
		ssh:        newSSHState(),
	}
}

// Start serves the gateway on an in-memory listener and returns a client connection to it.
// Stop shuts it down.
func (g *Gateway) Start() (*grpc.ClientConn, error) {
	g.lis = bufconn.Listen(1 << 20)
	g.srv = grpc.NewServer()
	openshellv1.RegisterOpenShellServer(g.srv, g)
	go func() { _ = g.srv.Serve(g.lis) }()
	return grpc.NewClient("passthrough:///fakegateway",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return g.lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

// Stop stops the server, ending open streams.
func (g *Gateway) Stop() {
	if g.srv != nil {
		g.srv.Stop()
	}
}

// Handle registers cmd for exec requests whose first argument is name.
func (g *Gateway) Handle(name string, cmd Command) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commands[name] = cmd
}

// FailNext makes the next call to method (for example "CreateSandbox") return err.
// Errors queue up in order.
func (g *Gateway) FailNext(method string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.faults[method] = append(g.faults[method], err)
}

// Calls returns how often method has been called.
func (g *Gateway) Calls(method string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[method]
}

// Provider returns a copy of a stored provider record, credentials included.
func (g *Gateway) Provider(workspace, name string) *datamodelv1.Provider {
	g.mu.Lock()
	defer g.mu.Unlock()
	return clone(g.providers[key{workspace, name}])
}

// Profile returns a copy of a stored provider profile.
func (g *Gateway) Profile(workspace, id string) *openshellv1.ProviderProfile {
	g.mu.Lock()
	defer g.mu.Unlock()
	return clone(g.profiles[key{workspace, id}])
}

// Sandbox returns a copy of a stored sandbox.
func (g *Gateway) Sandbox(workspace, name string) *openshellv1.Sandbox {
	g.mu.Lock()
	defer g.mu.Unlock()
	return clone(g.sandboxes[key{workspace, name}])
}

// Policies returns copies of a sandbox's policy revisions, oldest first.
func (g *Gateway) Policies(workspace, sandbox string) []*sandboxv1.SandboxPolicy {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*sandboxv1.SandboxPolicy, 0, len(g.policies[key{workspace, sandbox}]))
	for _, p := range g.policies[key{workspace, sandbox}] {
		out = append(out, clone(p))
	}
	return out
}

// Execs returns copies of the exec start requests received, oldest first.
func (g *Gateway) Execs() []*openshellv1.ExecSandboxRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*openshellv1.ExecSandboxRequest, 0, len(g.execs))
	for _, e := range g.execs {
		out = append(out, clone(e))
	}
	return out
}

// SetPhase forces a sandbox's phase, for example to simulate an infrastructure stop.
func (g *Gateway) SetPhase(workspace, name string, phase openshellv1.SandboxPhase) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sb := g.sandboxes[key{workspace, name}]; sb != nil {
		sb.Status.Phase = phase
	}
}

// SetCondition sets a sandbox's first condition to status False with reason and message, to
// simulate an image pull failure, an admission refusal or an unschedulable pod.
func (g *Gateway) SetCondition(workspace, name, reason, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sb := g.sandboxes[key{workspace, name}]; sb != nil {
		sb.Status.Conditions = []*openshellv1.SandboxCondition{{
			Type: "Ready", Status: "False", Reason: reason, Message: message,
		}}
	}
}

func clone[T proto.Message](m T) T {
	if any(m) == nil {
		return m
	}
	return proto.Clone(m).(T)
}

// enter counts a call and returns a queued fault. The caller holds g.mu.
func (g *Gateway) enter(method string) error {
	g.calls[method]++
	if q := g.faults[method]; len(q) > 0 {
		g.faults[method] = q[1:]
		return q[0]
	}
	return nil
}

func (g *Gateway) meta(name, workspace string, labels map[string]string) *datamodelv1.ObjectMeta {
	g.seq++
	return &datamodelv1.ObjectMeta{
		Id: fmt.Sprintf("id-%d", g.seq), Name: name, Workspace: workspace,
		Labels: maps.Clone(labels), ResourceVersion: 1,
	}
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func workspaceOf(sel *datamodelv1.WorkspaceSelector) (string, error) {
	ws := sel.GetWorkspace()
	if ws == "" {
		return "", status.Error(codes.InvalidArgument, "workspace_scope must name one workspace")
	}
	return ws, nil
}

// GetWorkspace implements openshellv1.OpenShellServer.
func (g *Gateway) GetWorkspace(_ context.Context, req *openshellv1.GetWorkspaceRequest) (*openshellv1.GetWorkspaceResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetWorkspace"); err != nil {
		return nil, err
	}
	w, ok := g.workspaces[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "workspace %q not found", req.GetName())
	}
	return &openshellv1.GetWorkspaceResponse{Workspace: clone(w)}, nil
}

// CreateWorkspace implements openshellv1.OpenShellServer.
func (g *Gateway) CreateWorkspace(_ context.Context, req *openshellv1.CreateWorkspaceRequest) (*openshellv1.CreateWorkspaceResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateWorkspace"); err != nil {
		return nil, err
	}
	if !dnsLabel.MatchString(req.GetName()) {
		return nil, status.Error(codes.InvalidArgument, "workspace name must be a DNS-1123 label")
	}
	if _, ok := g.workspaces[req.GetName()]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "workspace %q already exists", req.GetName())
	}
	w := &datamodelv1.Workspace{
		Metadata: g.meta(req.GetName(), "", req.GetLabels()),
		Status:   &datamodelv1.WorkspaceStatus{Phase: datamodelv1.WorkspacePhase_WORKSPACE_PHASE_ACTIVE},
	}
	g.workspaces[req.GetName()] = w
	return &openshellv1.CreateWorkspaceResponse{Workspace: clone(w)}, nil
}

// GetProviderProfile implements openshellv1.OpenShellServer.
func (g *Gateway) GetProviderProfile(_ context.Context, req *openshellv1.GetProviderProfileRequest) (*openshellv1.ProviderProfileResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetProviderProfile"); err != nil {
		return nil, err
	}
	p, ok := g.profiles[key{req.GetWorkspaceScope().GetWorkspace(), req.GetId()}]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider profile %q not found", req.GetId())
	}
	return &openshellv1.ProviderProfileResponse{Profile: clone(p)}, nil
}

// LintProviderProfiles implements openshellv1.OpenShellServer.
func (g *Gateway) LintProviderProfiles(_ context.Context, req *openshellv1.LintProviderProfilesRequest) (*openshellv1.LintProviderProfilesResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("LintProviderProfiles"); err != nil {
		return nil, err
	}
	diags := lint(req.GetProfiles())
	return &openshellv1.LintProviderProfilesResponse{
		Diagnostics: diags, Valid: len(diags) == 0, SupportsStablePlaceholder: !g.NoStablePlaceholder,
	}, nil
}

func lint(items []*openshellv1.ProviderProfileImportItem) []*openshellv1.ProviderProfileDiagnostic {
	var out []*openshellv1.ProviderProfileDiagnostic
	for _, it := range items {
		for _, e := range it.GetProfile().GetEndpoints() {
			if e.GetProtocol() != "" && len(e.GetRules()) == 0 && e.GetAccess() == sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_UNSPECIFIED {
				out = append(out, &openshellv1.ProviderProfileDiagnostic{
					Source: it.GetSource(), ProfileId: it.GetProfile().GetId(), Field: "endpoints",
					Message: "protocol requires rules or access to define allowed traffic", Severity: "error",
				})
			}
		}
	}
	return out
}

func (g *Gateway) storeProfile(ws string, p *openshellv1.ProviderProfile, version uint64) *openshellv1.ProviderProfile {
	p = clone(p)
	p.ResourceVersion = version
	p.Source, p.Scope = "user", "workspace"
	if g.NoStablePlaceholder {
		for _, c := range p.GetCredentials() {
			c.StablePlaceholder = false
		}
	}
	g.profiles[key{ws, p.GetId()}] = p
	return clone(p)
}

// ImportProviderProfiles implements openshellv1.OpenShellServer.
func (g *Gateway) ImportProviderProfiles(_ context.Context, req *openshellv1.ImportProviderProfilesRequest) (*openshellv1.ImportProviderProfilesResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("ImportProviderProfiles"); err != nil {
		return nil, err
	}
	ws, err := workspaceOf(req.GetWorkspaceScope())
	if err != nil {
		return nil, err
	}
	if diags := lint(req.GetProfiles()); len(diags) > 0 {
		return &openshellv1.ImportProviderProfilesResponse{Diagnostics: diags}, nil
	}
	resp := &openshellv1.ImportProviderProfilesResponse{Imported: true}
	for _, it := range req.GetProfiles() {
		if _, ok := g.profiles[key{ws, it.GetProfile().GetId()}]; ok {
			return nil, status.Errorf(codes.AlreadyExists, "provider profile %q already exists", it.GetProfile().GetId())
		}
	}
	for _, it := range req.GetProfiles() {
		resp.Profiles = append(resp.Profiles, g.storeProfile(ws, it.GetProfile(), 1))
	}
	return resp, nil
}

// UpdateProviderProfiles implements openshellv1.OpenShellServer.
func (g *Gateway) UpdateProviderProfiles(_ context.Context, req *openshellv1.UpdateProviderProfilesRequest) (*openshellv1.UpdateProviderProfilesResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("UpdateProviderProfiles"); err != nil {
		return nil, err
	}
	ws, err := workspaceOf(req.GetWorkspaceScope())
	if err != nil {
		return nil, err
	}
	cur, ok := g.profiles[key{ws, req.GetId()}]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider profile %q not found", req.GetId())
	}
	if req.GetProfile().GetProfile().GetId() != req.GetId() {
		return nil, status.Error(codes.InvalidArgument, "profile id does not match")
	}
	expected := req.GetExpectedResourceVersion()
	if expected == 0 {
		expected = req.GetProfile().GetProfile().GetResourceVersion()
	}
	if expected == 0 {
		return nil, status.Error(codes.InvalidArgument, "expected_resource_version is required")
	}
	if expected != cur.GetResourceVersion() {
		return nil, status.Errorf(codes.Aborted, "resource version %d does not match %d", expected, cur.GetResourceVersion())
	}
	if diags := lint([]*openshellv1.ProviderProfileImportItem{req.GetProfile()}); len(diags) > 0 {
		return &openshellv1.UpdateProviderProfilesResponse{Diagnostics: diags}, nil
	}
	p := g.storeProfile(ws, req.GetProfile().GetProfile(), cur.GetResourceVersion()+1)
	return &openshellv1.UpdateProviderProfilesResponse{Profile: p, Updated: true}, nil
}

// redacted returns a provider as the gateway returns it: credential keys, no values.
func redacted(p *datamodelv1.Provider) *datamodelv1.Provider {
	p = clone(p)
	for k := range p.GetCredentials() {
		p.Credentials[k] = ""
	}
	return p
}

// GetProvider implements openshellv1.OpenShellServer.
func (g *Gateway) GetProvider(_ context.Context, req *openshellv1.GetProviderRequest) (*openshellv1.ProviderResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetProvider"); err != nil {
		return nil, err
	}
	p, ok := g.providers[key{req.GetWorkspaceScope().GetWorkspace(), req.GetName()}]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider %q not found", req.GetName())
	}
	return &openshellv1.ProviderResponse{Provider: redacted(p)}, nil
}

// CreateProvider implements openshellv1.OpenShellServer.
func (g *Gateway) CreateProvider(_ context.Context, req *openshellv1.CreateProviderRequest) (*openshellv1.ProviderResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateProvider"); err != nil {
		return nil, err
	}
	ws, err := workspaceOf(req.GetWorkspaceScope())
	if err != nil {
		return nil, err
	}
	in := req.GetProvider()
	name := in.GetMetadata().GetName()
	if _, ok := g.providers[key{ws, name}]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "provider %q already exists", name)
	}
	if _, ok := g.profiles[key{in.GetProfileWorkspace(), in.GetType()}]; !ok {
		return nil, status.Errorf(codes.NotFound, "provider profile %q not found", in.GetType())
	}
	for k, v := range in.GetConfig() {
		if v == "" {
			return nil, status.Errorf(codes.InvalidArgument, "provider.config[%q] is empty", k)
		}
	}
	p := clone(in)
	p.Metadata = g.meta(name, ws, in.GetMetadata().GetLabels())
	g.providers[key{ws, name}] = p
	return &openshellv1.ProviderResponse{Provider: redacted(p)}, nil
}

func merge(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		if v == "" {
			delete(dst, k)
		} else {
			dst[k] = v
		}
	}
	return dst
}

// UpdateProvider implements openshellv1.OpenShellServer.
func (g *Gateway) UpdateProvider(_ context.Context, req *openshellv1.UpdateProviderRequest) (*openshellv1.ProviderResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("UpdateProvider"); err != nil {
		return nil, err
	}
	ws, err := workspaceOf(req.GetWorkspaceScope())
	if err != nil {
		return nil, err
	}
	in := req.GetProvider()
	cur, ok := g.providers[key{ws, in.GetMetadata().GetName()}]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider %q not found", in.GetMetadata().GetName())
	}
	if in.GetType() != "" && in.GetType() != cur.GetType() {
		return nil, status.Error(codes.InvalidArgument, "provider type cannot be changed; delete and recreate the provider")
	}
	cur.Credentials = merge(cur.Credentials, in.GetCredentials())
	cur.Config = merge(cur.Config, in.GetConfig())
	cur.Metadata.ResourceVersion++
	return &openshellv1.ProviderResponse{Provider: redacted(cur)}, nil
}

// DeleteProvider implements openshellv1.OpenShellServer.
func (g *Gateway) DeleteProvider(_ context.Context, req *openshellv1.DeleteProviderRequest) (*openshellv1.DeleteProviderResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteProvider"); err != nil {
		return nil, err
	}
	k := key{req.GetWorkspaceScope().GetWorkspace(), req.GetName()}
	if _, ok := g.providers[k]; !ok {
		if req.GetAllowMissing() {
			return &openshellv1.DeleteProviderResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_ALREADY_ABSENT}, nil
		}
		return nil, status.Errorf(codes.NotFound, "provider %q not found", req.GetName())
	}
	delete(g.providers, k)
	return &openshellv1.DeleteProviderResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
}

// DeleteProviderProfile implements openshellv1.OpenShellServer. Like OpenShell, it refuses to
// delete a profile that a provider still uses.
func (g *Gateway) DeleteProviderProfile(_ context.Context, req *openshellv1.DeleteProviderProfileRequest) (*openshellv1.DeleteProviderProfileResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteProviderProfile"); err != nil {
		return nil, err
	}
	ws := req.GetWorkspaceScope().GetWorkspace()
	k := key{ws, req.GetId()}
	if _, ok := g.profiles[k]; !ok {
		if req.GetAllowMissing() {
			return &openshellv1.DeleteProviderProfileResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_ALREADY_ABSENT}, nil
		}
		return nil, status.Errorf(codes.NotFound, "provider profile %q not found", req.GetId())
	}
	for pk, p := range g.providers {
		if pk.workspace == ws && p.GetType() == req.GetId() {
			return nil, status.Errorf(codes.FailedPrecondition, "provider profile %q is used by provider %q", req.GetId(), p.GetMetadata().GetName())
		}
	}
	delete(g.profiles, k)
	return &openshellv1.DeleteProviderProfileResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
}

// CreateSandbox implements openshellv1.OpenShellServer. New sandboxes are ready at once.
func (g *Gateway) CreateSandbox(_ context.Context, req *openshellv1.CreateSandboxRequest) (*openshellv1.SandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateSandbox"); err != nil {
		return nil, err
	}
	ws, err := workspaceOf(req.GetWorkspaceScope())
	if err != nil {
		return nil, err
	}
	if _, ok := g.workspaces[ws]; !ok {
		return nil, status.Errorf(codes.NotFound, "workspace %q not found", ws)
	}
	k := key{ws, req.GetName()}
	if _, ok := g.sandboxes[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "sandbox %q already exists", req.GetName())
	}
	if req.GetSpec().GetPolicy() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec.policy is required")
	}
	for _, p := range req.GetSpec().GetProviders() {
		if _, ok := g.providers[key{ws, p}]; !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "provider %q not found", p)
		}
	}
	sb := &openshellv1.Sandbox{
		Metadata: g.meta(req.GetName(), ws, req.GetLabels()),
		Spec:     clone(req.GetSpec()),
		Status: &openshellv1.SandboxStatus{
			Phase:                openshellv1.SandboxPhase_SANDBOX_PHASE_READY,
			CurrentPolicyVersion: 1,
		},
	}
	g.sandboxes[k] = sb
	g.policies[k] = []*sandboxv1.SandboxPolicy{clone(req.GetSpec().GetPolicy())}
	return &openshellv1.SandboxResponse{Sandbox: clone(sb)}, nil
}

func (g *Gateway) sandbox(sel *datamodelv1.WorkspaceSelector, name string) (*openshellv1.Sandbox, key, error) {
	k := key{sel.GetWorkspace(), name}
	sb, ok := g.sandboxes[k]
	if !ok {
		return nil, k, status.Errorf(codes.NotFound, "sandbox %q not found", name)
	}
	return sb, k, nil
}

// GetSandbox implements openshellv1.OpenShellServer.
func (g *Gateway) GetSandbox(_ context.Context, req *openshellv1.GetSandboxRequest) (*openshellv1.SandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetSandbox"); err != nil {
		return nil, err
	}
	sb, _, err := g.sandbox(req.GetWorkspaceScope(), req.GetName())
	if err != nil {
		return nil, err
	}
	return &openshellv1.SandboxResponse{Sandbox: clone(sb)}, nil
}

// StopSandbox implements openshellv1.OpenShellServer.
func (g *Gateway) StopSandbox(_ context.Context, req *openshellv1.StopSandboxRequest) (*openshellv1.SandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("StopSandbox"); err != nil {
		return nil, err
	}
	sb, _, err := g.sandbox(req.GetWorkspaceScope(), req.GetName())
	if err != nil {
		return nil, err
	}
	if sb.GetStatus().GetPhase() != openshellv1.SandboxPhase_SANDBOX_PHASE_READY {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox is %s, not ready", sb.GetStatus().GetPhase())
	}
	sb.Status.Phase = openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED
	return &openshellv1.SandboxResponse{Sandbox: clone(sb)}, nil
}

// StartSandbox implements openshellv1.OpenShellServer.
func (g *Gateway) StartSandbox(_ context.Context, req *openshellv1.StartSandboxRequest) (*openshellv1.SandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("StartSandbox"); err != nil {
		return nil, err
	}
	sb, _, err := g.sandbox(req.GetWorkspaceScope(), req.GetName())
	if err != nil {
		return nil, err
	}
	if sb.GetStatus().GetPhase() != openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox is %s, not stopped", sb.GetStatus().GetPhase())
	}
	sb.Status.Phase = openshellv1.SandboxPhase_SANDBOX_PHASE_READY
	return &openshellv1.SandboxResponse{Sandbox: clone(sb)}, nil
}

// DeleteSandbox implements openshellv1.OpenShellServer.
func (g *Gateway) DeleteSandbox(_ context.Context, req *openshellv1.DeleteSandboxRequest) (*openshellv1.DeleteSandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteSandbox"); err != nil {
		return nil, err
	}
	sb, k, err := g.sandbox(req.GetWorkspaceScope(), req.GetName())
	if err != nil {
		if req.GetAllowMissing() {
			return &openshellv1.DeleteSandboxResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_ALREADY_ABSENT}, nil
		}
		return nil, err
	}
	delete(g.sandboxes, k)
	delete(g.policies, k)
	return &openshellv1.DeleteSandboxResponse{
		Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_COMPLETED, SandboxId: sb.GetMetadata().GetId(),
	}, nil
}

// UpdateConfig implements openshellv1.OpenShellServer for sandbox-scoped policy updates.
func (g *Gateway) UpdateConfig(_ context.Context, req *openshellv1.UpdateConfigRequest) (*openshellv1.UpdateConfigResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("UpdateConfig"); err != nil {
		return nil, err
	}
	if req.GetGlobal() || req.GetPolicy() == nil {
		return nil, status.Error(codes.Unimplemented, "fakegateway supports sandbox-scoped policy updates only")
	}
	sb, k, err := g.sandbox(req.GetWorkspaceScope(), req.GetSandbox())
	if err != nil {
		return nil, err
	}
	revs := g.policies[k]
	base := revs[0]
	next := req.GetPolicy()
	if !proto.Equal(base.GetFilesystem(), next.GetFilesystem()) ||
		!proto.Equal(base.GetLandlock(), next.GetLandlock()) ||
		!proto.Equal(base.GetProcess(), next.GetProcess()) {
		return nil, status.Error(codes.InvalidArgument, "only network_policies may change on a running sandbox")
	}
	if !proto.Equal(revs[len(revs)-1], next) {
		g.policies[k] = append(revs, clone(next))
	}
	version := uint32(len(g.policies[k])) //nolint:gosec // a handful of revisions in tests
	sb.Status.CurrentPolicyVersion = version
	return &openshellv1.UpdateConfigResponse{Version: version}, nil
}

// ExecSandboxInteractive implements openshellv1.OpenShellServer, without a TTY.
func (g *Gateway) ExecSandboxInteractive(stream grpc.BidiStreamingServer[openshellv1.ExecSandboxInput, openshellv1.ExecSandboxEvent]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to read first message: %v", err)
	}
	req := first.GetStart()
	cmd, err := g.startExec(req)
	if err != nil {
		return err
	}
	if err := stream.SendHeader(nil); err != nil {
		return err
	}

	var sendMu sync.Mutex
	send := func(ev *openshellv1.ExecSandboxEvent) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(ev)
	}
	// Stdin is queued as the supervisor queues it, with its cap on what the command has not read:
	// a command that reads slower than the client sends is killed with exit status 74 and no
	// stderr (supervisor ssh/input.rs MAX_PENDING_INPUT, ssh.rs reject_exec_input), as OpenShell
	// 0.1.3 and later do. Credit cannot help a client: SSH grants it before the data is read.
	ectx, cancelExec := context.WithCancel(stream.Context())
	defer cancelExec()
	stdinQ := newInputQueue()
	// A rejection only applies while the command runs, and the exit status is sent once.
	var stateMu sync.Mutex
	running, rejected := true, false
	go func() {
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				stdinQ.closeInput()
				return
			}
			if err != nil {
				stdinQ.fail(err)
				return
			}
			data := msg.GetStdin()
			if data == nil {
				if msg.GetResize() != nil {
					continue
				}
				stdinQ.fail(errors.New("expected stdin or resize after exec start"))
				return
			}
			if stdinQ.pendingBytes()+len(data) > MaxPendingStdin {
				// The exit status goes out before the command is stopped, or the handler could
				// end the stream first.
				stateMu.Lock()
				if !running {
					stateMu.Unlock()
					return
				}
				rejected = true
				_ = send(&openshellv1.ExecSandboxEvent{Payload: &openshellv1.ExecSandboxEvent_Exit{Exit: &openshellv1.ExecSandboxExit{ExitCode: 74}}})
				stateMu.Unlock()
				stdinQ.fail(errors.New("stdin queue full"))
				cancelExec()
				return
			}
			stdinQ.push(bytes.Clone(data))
		}
	}()

	code := cmd(ectx, req.GetCommand()[1:], stdinQ,
		eventWriter(send, func(b []byte) *openshellv1.ExecSandboxEvent {
			return &openshellv1.ExecSandboxEvent{Payload: &openshellv1.ExecSandboxEvent_Stdout{Stdout: &openshellv1.ExecSandboxStdout{Data: b}}}
		}),
		eventWriter(send, func(b []byte) *openshellv1.ExecSandboxEvent {
			return &openshellv1.ExecSandboxEvent{Payload: &openshellv1.ExecSandboxEvent_Stderr{Stderr: &openshellv1.ExecSandboxStderr{Data: b}}}
		}),
	)
	stateMu.Lock()
	running = false
	wasRejected := rejected
	stateMu.Unlock()
	if wasRejected {
		return nil // the exit status 74 is sent
	}
	// Unblock the stdin goroutine if the command stopped reading.
	stdinQ.fail(io.ErrClosedPipe)
	if err := stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return send(&openshellv1.ExecSandboxEvent{Payload: &openshellv1.ExecSandboxEvent_Exit{
		Exit: &openshellv1.ExecSandboxExit{ExitCode: int32(code)}, //nolint:gosec // exit codes are small
	}})
}

func (g *Gateway) startExec(req *openshellv1.ExecSandboxRequest) (Command, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("ExecSandboxInteractive"); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "first message must be start")
	}
	if len(req.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}
	if req.GetTty() {
		return nil, status.Error(codes.Unimplemented, "fakegateway exec has no TTY")
	}
	g.execs = append(g.execs, clone(req))
	sb, _, err := g.sandbox(req.GetWorkspaceScope(), req.GetSandbox())
	if err != nil {
		return nil, err
	}
	if sb.GetStatus().GetPhase() != openshellv1.SandboxPhase_SANDBOX_PHASE_READY {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is not ready")
	}
	cmd, ok := g.commands[req.GetCommand()[0]]
	if !ok {
		name := req.GetCommand()[0]
		cmd = func(_ context.Context, _ []string, _ io.Reader, _, stderr io.Writer) int {
			_, _ = fmt.Fprintf(stderr, "%s: command not found\n", name)
			return 127
		}
	}
	return cmd, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func eventWriter(send func(*openshellv1.ExecSandboxEvent) error, wrap func([]byte) *openshellv1.ExecSandboxEvent) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		// The supervisor sends output in small messages, never one write as one message.
		n := 0
		for len(b) > 0 {
			c := b[:min(len(b), 32<<10)]
			if err := send(wrap(append([]byte(nil), c...))); err != nil {
				return n, err
			}
			n += len(c)
			b = b[len(c):]
		}
		return n, nil
	})
}
