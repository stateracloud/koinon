// SPDX-License-Identifier: Apache-2.0

// Package openshell implements execution.Backend on the OpenShell gateway's gRPC API
// (openshell.v1.OpenShell, v0.1.7), through the client generated from the vendored protos
// in third_party/openshell (ADR 0048: generated client, not sdk/go).
package openshell

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/stateracloud/koinon/internal/execution"
	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// Config is how to reach the gateway. The gateway authenticates clients with mTLS by
// default; the files are the client bundle OpenShell issues.
type Config struct {
	// Endpoint is the gateway's host:port.
	Endpoint string
	// CertFile and KeyFile are the client certificate and key.
	CertFile string
	KeyFile  string
	// CAFile is the CA that signed the gateway's certificate.
	CAFile string
	// ServerName overrides the name checked against the gateway's certificate.
	ServerName string
}

// Backend is the OpenShell implementation of execution.Backend.
type Backend struct {
	c    openshellv1.OpenShellClient
	conn *grpc.ClientConn
	// profileMu serialises ensureProfile: the profile of a flavor is shared by the agents of a
	// workspace, which the reconciler converges concurrently (plan item LT1), and a profile
	// imported twice at once is an AlreadyExists error.
	profileMu sync.Mutex
	// owner is the value of execution.LabelInstall this Backend stamps and enforces (WithOwner).
	owner string
}

var _ execution.Backend = (*Backend)(nil)

// New returns a Backend on an existing connection. The caller owns conn.
func New(conn grpc.ClientConnInterface) *Backend {
	return &Backend{c: openshellv1.NewOpenShellClient(conn)}
}

// Dial connects to the gateway with mTLS. Close releases the connection.
func Dial(cfg Config, opts ...grpc.DialOption) (*Backend, error) {
	tlsCfg, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}, opts...)
	conn, err := grpc.NewClient(cfg.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("openshell: connect to %s: %w", cfg.Endpoint, err)
	}
	b := New(conn)
	b.conn = conn
	return b, nil
}

// DialPlaintext connects to the gateway at endpoint without TLS, for a gateway behind a mesh
// that terminates mTLS. Close closes the connection.
func DialPlaintext(endpoint string, opts ...grpc.DialOption) (*Backend, error) {
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("openshell: connect to %s: %w", endpoint, err)
	}
	b := New(conn)
	b.conn = conn
	return b, nil
}

// Close closes the connection opened by Dial or DialPlaintext. It does nothing for a Backend
// from New.
func (b *Backend) Close() error {
	if b.conn == nil {
		return nil
	}
	return b.conn.Close()
}

// Health calls the gateway's unauthenticated Health RPC, which reports its status and
// version (the install preflight checks both).
func (b *Backend) Health(ctx context.Context) (*openshellv1.HealthResponse, error) {
	return b.c.Health(ctx, &openshellv1.HealthRequest{})
}

func (cfg Config) tlsConfig() (*tls.Config, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" || cfg.CAFile == "" {
		return nil, errors.New("openshell: CertFile, KeyFile and CAFile are required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("openshell: load client certificate: %w", err)
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("openshell: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("openshell: no certificates in %s", cfg.CAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   cfg.ServerName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func scope(workspace string) *datamodelv1.WorkspaceSelector {
	return &datamodelv1.WorkspaceSelector{Selection: &datamodelv1.WorkspaceSelector_Workspace{Workspace: workspace}}
}

// EnsureWorkspace implements execution.Backend.
func (b *Backend) EnsureWorkspace(ctx context.Context, name string, labels map[string]string) (execution.Workspace, error) {
	const op = "EnsureWorkspace"
	for range 2 {
		got, err := b.c.GetWorkspace(ctx, &openshellv1.GetWorkspaceRequest{Name: name})
		if err == nil {
			w := workspaceFromProto(got.GetWorkspace())
			return w, b.check(op, "workspace", name, w.Labels)
		}
		if !errors.Is(wrap(op, err), execution.ErrNotFound) {
			return execution.Workspace{}, wrap(op, err)
		}
		created, err := b.c.CreateWorkspace(ctx, &openshellv1.CreateWorkspaceRequest{Name: name, Labels: b.stamp(labels)})
		if err == nil {
			return workspaceFromProto(created.GetWorkspace()), nil
		}
		if !errors.Is(wrap(op, err), execution.ErrAlreadyExists) {
			return execution.Workspace{}, wrap(op, err)
		}
		// Created concurrently: read it on the next pass.
	}
	return execution.Workspace{}, &execution.Error{Op: op, Kind: execution.ErrConflict, Err: errors.New("workspace keeps changing")}
}

// DeleteWorkspace removes a workspace record; a workspace that is already gone is not an error.
// It is not part of execution.Backend: the escape suite uses it to leave no record behind.
func (b *Backend) DeleteWorkspace(ctx context.Context, name string) error {
	_, err := b.c.DeleteWorkspace(ctx, &openshellv1.DeleteWorkspaceRequest{Name: name, AllowMissing: true})
	return wrap("DeleteWorkspace", err)
}

func workspaceFromProto(w *datamodelv1.Workspace) execution.Workspace {
	return execution.Workspace{
		Name:   w.GetMetadata().GetName(),
		ID:     w.GetMetadata().GetId(),
		Labels: w.GetMetadata().GetLabels(),
		Active: w.GetStatus().GetPhase() == datamodelv1.WorkspacePhase_WORKSPACE_PHASE_ACTIVE,
	}
}

// CreateSandbox implements execution.Backend.
func (b *Backend) CreateSandbox(ctx context.Context, workspace string, spec execution.SandboxSpec) (execution.Sandbox, error) {
	const op = "CreateSandbox"
	spec.Labels = b.stamp(spec.Labels)
	req, err := renderSandbox(workspace, spec)
	if err != nil {
		return execution.Sandbox{}, &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: err}
	}
	resp, err := b.c.CreateSandbox(ctx, req)
	if err == nil {
		return sandboxFromProto(resp.GetSandbox()), nil
	}
	if errors.Is(wrap(op, err), execution.ErrAlreadyExists) {
		// GetSandbox refuses a sandbox another install owns.
		return b.GetSandbox(ctx, workspace, spec.Name)
	}
	return execution.Sandbox{}, wrap(op, err)
}

// GetSandbox implements execution.Backend. A sandbox another install owns is refused with
// [execution.ErrForeign] (see [Backend.WithOwner]), so no caller records or trusts it.
func (b *Backend) GetSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	resp, err := b.c.GetSandbox(ctx, &openshellv1.GetSandboxRequest{WorkspaceScope: scope(workspace), Name: name})
	if err != nil {
		return execution.Sandbox{}, wrap("GetSandbox", err)
	}
	sb := sandboxFromProto(resp.GetSandbox())
	if err := b.check("GetSandbox", "sandbox", name, sb.Labels); err != nil {
		return execution.Sandbox{}, err
	}
	return sb, nil
}

// StartSandbox implements execution.Backend.
func (b *Backend) StartSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	if err := b.guardSandbox(ctx, workspace, name); err != nil {
		return execution.Sandbox{}, err
	}
	resp, err := b.c.StartSandbox(ctx, &openshellv1.StartSandboxRequest{WorkspaceScope: scope(workspace), Name: name})
	if err == nil {
		return sandboxFromProto(resp.GetSandbox()), nil
	}
	return b.settle(ctx, "StartSandbox", workspace, name, err,
		execution.PhaseReady, execution.PhaseStarting, execution.PhaseProvisioning)
}

// StopSandbox implements execution.Backend.
func (b *Backend) StopSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	if err := b.guardSandbox(ctx, workspace, name); err != nil {
		return execution.Sandbox{}, err
	}
	resp, err := b.c.StopSandbox(ctx, &openshellv1.StopSandboxRequest{WorkspaceScope: scope(workspace), Name: name})
	if err == nil {
		return sandboxFromProto(resp.GetSandbox()), nil
	}
	return b.settle(ctx, "StopSandbox", workspace, name, err,
		execution.PhaseStopped, execution.PhaseStopping)
}

// settle turns a failed-precondition error from start or stop into success when the
// sandbox is already where the caller wants it.
func (b *Backend) settle(ctx context.Context, op, workspace, name string, err error, ok ...execution.Phase) (execution.Sandbox, error) {
	werr := wrap(op, err)
	if !errors.Is(werr, execution.ErrFailedPrecondition) {
		return execution.Sandbox{}, werr
	}
	sb, getErr := b.GetSandbox(ctx, workspace, name)
	if getErr != nil {
		return execution.Sandbox{}, werr
	}
	for _, p := range ok {
		if sb.Phase == p {
			return sb, nil
		}
	}
	return execution.Sandbox{}, werr
}

// DeleteSandbox implements execution.Backend.
func (b *Backend) DeleteSandbox(ctx context.Context, workspace, name string) error {
	if err := b.guardSandbox(ctx, workspace, name); err != nil {
		return err
	}
	_, err := b.c.DeleteSandbox(ctx, &openshellv1.DeleteSandboxRequest{
		WorkspaceScope: scope(workspace), Name: name, AllowMissing: true,
	})
	return wrap("DeleteSandbox", err)
}

// EnsurePolicy implements execution.Backend. The gateway deduplicates a policy whose
// canonical hash matches the latest revision, so an unchanged policy keeps its version.
func (b *Backend) EnsurePolicy(ctx context.Context, workspace, sandbox string, p execution.Policy) (uint32, error) {
	const op = "EnsurePolicy"
	if err := b.guardSandbox(ctx, workspace, sandbox); err != nil {
		return 0, err
	}
	policy, err := renderPolicy(p)
	if err != nil {
		return 0, &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: err}
	}
	resp, err := b.c.UpdateConfig(ctx, &openshellv1.UpdateConfigRequest{
		WorkspaceScope: scope(workspace), Sandbox: sandbox, Policy: policy,
	})
	if err != nil {
		return 0, wrap(op, err)
	}
	return resp.GetVersion(), nil
}

// DeleteProvider implements execution.Backend.
func (b *Backend) DeleteProvider(ctx context.Context, workspace, name string) error {
	if b.owner != "" {
		got, err := b.c.GetProvider(ctx, &openshellv1.GetProviderRequest{WorkspaceScope: scope(workspace), Name: name})
		if err == nil {
			if err := b.check("DeleteProvider", "provider", name, got.GetProvider().GetMetadata().GetLabels()); err != nil {
				return err
			}
		} else if !errors.Is(wrap("DeleteProvider", err), execution.ErrNotFound) {
			return wrap("DeleteProvider", err)
		}
	}
	_, err := b.c.DeleteProvider(ctx, &openshellv1.DeleteProviderRequest{
		WorkspaceScope: scope(workspace), Name: name, AllowMissing: true,
	})
	return wrap("DeleteProvider", err)
}

// DeleteProviderProfile implements execution.Backend.
func (b *Backend) DeleteProviderProfile(ctx context.Context, workspace, id string) error {
	if b.owner != "" {
		got, err := b.c.GetProviderProfile(ctx, &openshellv1.GetProviderProfileRequest{WorkspaceScope: scope(workspace), Id: id})
		if err == nil {
			if err := b.check("DeleteProviderProfile", "provider profile", id, got.GetProfile().GetAnnotations()); err != nil {
				return err
			}
		} else if !errors.Is(wrap("DeleteProviderProfile", err), execution.ErrNotFound) {
			return wrap("DeleteProviderProfile", err)
		}
	}
	_, err := b.c.DeleteProviderProfile(ctx, &openshellv1.DeleteProviderProfileRequest{
		WorkspaceScope: scope(workspace), Id: id, AllowMissing: true,
	})
	return wrap("DeleteProviderProfile", err)
}
