// SPDX-License-Identifier: Apache-2.0

// Package reconciler drives each agent toward its desired execution state (plan item R2):
//
//   - an active agent gets its org's OpenShell workspace (one per org, ADR 0002), an
//     agentgateway key issued through the ModelGateway (material in a Kubernetes Secret in
//     Koinon's namespace, only the hash in gateway config and Postgres), a provider record
//     holding that key behind a stable placeholder, and a sandbox on its flavor image with an
//     enforced policy (ADR 0045) and a persistent home;
//   - a sandbox stops after IdleTimeout without an active run (parked runs included) and starts
//     again when a run is queued or a park is resolved (ADR 0049);
//   - archiving deletes the sandbox, the provider record and the key; the home volume follows
//     HomeRetention;
//   - every pass re-applies everything, so hand edits are reverted, and every rendered object
//     carries owner labels.
//
// The observed sandbox state is written to agent_sandboxes, which koinon-agentd reads before it
// execs into a sandbox.
//
// # Loop
//
// The reconciler is level-triggered: each pass reads the whole desired state from Postgres
// (ADR 0003) and converges every object. Passes run on a timer (ResyncInterval) and on
// Postgres NOTIFY wake-ups from triggers on agents and runs, so a queued run starts its
// sandbox within a second rather than a resync interval. NOTIFY is only a hint (it is lost
// while no listener is connected); the timer covers missed hints, idle timeouts and drift. A
// work queue table was not used: a full pass is cheap at v0.1's scale, the gateway config is
// one document rendered from all agents anyway, and a queue would add claim and retry state
// without removing the need for a periodic drift pass. One replica reconciles at a time,
// holding a Postgres advisory lock on its listening connection.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
	"github.com/procoduck/koinon/internal/gateway"
)

// Retention decides what happens to an archived agent's home volume.
type Retention string

// Home retention values.
const (
	// RetentionDelete deletes the sandbox and with it the home volume (the default).
	RetentionDelete Retention = "delete"
	// RetentionRetain keeps the stopped sandbox, and so its home volume, after archive. Its
	// provider record and key are deleted; it never starts again. OpenShell 0.1.5 deletes the
	// workspace PVC together with its sandbox and offers no way to detach it, so keeping the
	// stopped sandbox is the only way to keep the volume through the gateway API.
	RetentionRetain Retention = "retain"
)

// Defaults.
const (
	DefaultIdleTimeout    = 10 * time.Minute
	DefaultResyncInterval = 30 * time.Second
)

// Endpoint is a host and port.
type Endpoint struct {
	Host string
	Port uint32
}

// Config configures a Reconciler.
type Config struct {
	// Gateway is agentgateway's LLM listener as sandboxes reach it.
	Gateway Endpoint
	// Models is the model upstream rendered into every gateway state.
	Models gateway.ModelUpstream
	// Flavors maps each flavor to its image and binaries. Agents of a flavor without an image
	// are reported as errors.
	Flavors map[store.AgentFlavor]Flavor
	// RuntimeClassName is the sandbox runtime class (ADR 0045: kata). Empty uses the
	// gateway's default.
	RuntimeClassName string
	// IdleTimeout stops a sandbox after this long without an active run. Default 10 minutes.
	IdleTimeout time.Duration
	// ResyncInterval is the time between passes without a wake-up. Default 30 seconds.
	ResyncInterval time.Duration
	// HomeRetention decides the home volume's fate on archive. Default RetentionDelete.
	HomeRetention Retention
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now; tests move it forward to pass the idle timeout.
	Now func() time.Time
}

// Reconciler reconciles agents into OpenShell sandboxes.
type Reconciler struct {
	cfg     Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	q       *store.Queries
	backend execution.Backend
	gw      gateway.ModelGateway
	secrets SecretStore
}

// New returns a Reconciler.
func New(pool *pgxpool.Pool, backend execution.Backend, gw gateway.ModelGateway, secrets SecretStore, cfg Config) (*Reconciler, error) {
	if cfg.Gateway.Host == "" || cfg.Gateway.Port == 0 {
		return nil, errors.New("reconciler: the agentgateway endpoint is required")
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.ResyncInterval == 0 {
		cfg.ResyncInterval = DefaultResyncInterval
	}
	switch cfg.HomeRetention {
	case "":
		cfg.HomeRetention = RetentionDelete
	case RetentionDelete, RetentionRetain:
	default:
		return nil, fmt.Errorf("reconciler: home retention must be %q or %q, not %q", RetentionDelete, RetentionRetain, cfg.HomeRetention)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reconciler{
		cfg: cfg, log: cfg.Logger, pool: pool, q: store.New(pool),
		backend: backend, gw: gw, secrets: secrets,
	}, nil
}

// errAbortPass marks errors after which applying the gateway state could revoke working keys.
var errAbortPass = errors.New("reconciler: pass aborted")

// work is one agent in a pass.
type work struct {
	org   store.ListOrgsToReconcileRow
	agent store.Agent
	sb    store.AgentSandbox
	// key and material are set for active agents whose key is in hand.
	key      gateway.Key
	material string
	keyErr   error
}

func (w *work) logAttrs() []any {
	return []any{"org", w.org.Slug, "agent", uuidString(w.agent.ID), "sandbox", w.sb.SandboxName}
}

// ReconcileAll runs one pass over every org and agent. It returns the errors of the agents it
// could not converge; the others are converged regardless.
func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	orgs, err := r.q.ListOrgsToReconcile(ctx)
	if err != nil {
		return fmt.Errorf("reconciler: list orgs: %w", err)
	}
	workspaces := map[string]string{} // org ID -> workspace, ensured in this pass
	var (
		items []*work
		keys  []gateway.Key
		errs  []error
	)
	for _, org := range orgs {
		agents, err := r.q.ListAgentsToReconcile(ctx, org.ID)
		if err != nil {
			// Without the full list, an Apply would revoke this org's keys.
			return fmt.Errorf("reconciler: list agents of org %s: %w", org.Slug, err)
		}
		for _, a := range agents {
			w, err := r.prepare(ctx, org, a, workspaces)
			if errors.Is(err, errAbortPass) {
				return err
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if w == nil {
				continue
			}
			items = append(items, w)
			if w.key.ID != "" {
				keys = append(keys, w.key)
			}
		}
	}

	// One gateway state for every agent: keys of archived agents are revoked by omission.
	state := gateway.State{Keys: keys, Models: r.cfg.Models}
	if _, err := r.gw.Apply(ctx, state); err != nil {
		errs = append(errs, fmt.Errorf("reconciler: apply gateway state: %w", err))
		return errors.Join(errs...)
	}

	for _, w := range items {
		if err := r.reconcileAgent(ctx, w, workspaces); err != nil {
			r.log.Warn("agent not converged", append(w.logAttrs(), "err", err)...)
			r.recordMessage(ctx, w, err)
			errs = append(errs, fmt.Errorf("agent %s: %w", uuidString(w.agent.ID), err))
		}
	}
	return errors.Join(errs...)
}

// prepare loads or creates the agent's row and, for an active agent, its key. It returns nil
// for agents with nothing rendered and nothing to render.
func (r *Reconciler) prepare(ctx context.Context, org store.ListOrgsToReconcileRow, a store.Agent, workspaces map[string]string) (*work, error) {
	w := &work{org: org, agent: a}
	sb, err := r.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org.ID, AgentID: a.ID})
	switch {
	case err == nil:
		w.sb = sb
	case errors.Is(err, pgx.ErrNoRows):
		if a.Status != store.AgentStatusActive {
			return nil, nil
		}
		ws, err := r.ensureWorkspace(ctx, org, workspaces)
		if err != nil {
			return nil, err
		}
		names := AgentNames(org.ID, a.ID)
		w.sb, err = r.q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
			OrgID: org.ID, AgentID: a.ID, Workspace: ws,
			SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
		})
		if err != nil {
			return nil, fmt.Errorf("reconciler: record sandbox of agent %s: %w", uuidString(a.ID), err)
		}
	default:
		// Without the row, the agent's key would be left out of the gateway state and so
		// revoked: abort the pass instead.
		return nil, fmt.Errorf("%w: read sandbox of agent %s: %w", errAbortPass, uuidString(a.ID), err)
	}

	if recorded, ok := recordedKey(org, w.sb); ok {
		// Keep the recorded key in the gateway state unless a newer one replaces it, so a
		// failure below never revokes a working key. Draft agents keep theirs for a quick
		// re-activation; archived agents' keys are left out and so revoked.
		if a.Status != store.AgentStatusArchived {
			w.key = recorded
		}
	}
	if a.Status == store.AgentStatusActive {
		key, material, err := r.ensureKey(ctx, w)
		if err != nil {
			w.keyErr = err
		} else {
			w.key, w.material = key, material
		}
	}
	return w, nil
}

func agentRef(org store.ListOrgsToReconcileRow, agent pgtype.UUID) gateway.AgentRef {
	return gateway.AgentRef{Org: org.Slug, Name: uuidString(agent)}
}

func recordedKey(org store.ListOrgsToReconcileRow, sb store.AgentSandbox) (gateway.Key, bool) {
	if !sb.GatewayKeyID.Valid || !sb.GatewayKeyHash.Valid {
		return gateway.Key{}, false
	}
	return gateway.Key{ID: sb.GatewayKeyID.String, Agent: agentRef(org, sb.AgentID), Hash: sb.GatewayKeyHash.String}, true
}

// ensureKey returns the agent's key and its material from the key Secret, issuing a new key
// when there is none or the Secret no longer matches the recorded hash (a hand edit or a
// deleted Secret: the old material is gone, so the key is rotated).
func (r *Reconciler) ensureKey(ctx context.Context, w *work) (gateway.Key, string, error) {
	sec, found, err := r.secrets.Get(ctx, w.sb.KeySecretName)
	if err != nil {
		return gateway.Key{}, "", err
	}
	if recorded, ok := recordedKey(w.org, w.sb); ok && found &&
		sec.KeyID == recorded.ID && gateway.HashKey(sec.Material) == recorded.Hash {
		return recorded, sec.Material, nil
	}
	reason := "no key yet"
	if w.sb.GatewayKeyID.Valid {
		reason = "key Secret missing or not matching the recorded hash"
	}

	issued, err := r.gw.IssueKey(ctx, agentRef(w.org, w.agent.ID))
	if err != nil {
		return gateway.Key{}, "", fmt.Errorf("issue gateway key: %w", err)
	}
	if err := r.secrets.Put(ctx, KeySecret{
		Name: w.sb.KeySecretName, KeyID: issued.ID, KeyHash: issued.Hash, Material: issued.Material,
		Labels: ownerLabels(w.org.ID, w.agent.ID),
	}); err != nil {
		return gateway.Key{}, "", err
	}
	sb, err := r.q.SetAgentSandboxKey(ctx, store.SetAgentSandboxKeyParams{
		OrgID: w.org.ID, AgentID: w.agent.ID,
		GatewayKeyID:   pgtype.Text{String: issued.ID, Valid: true},
		GatewayKeyHash: pgtype.Text{String: issued.Hash, Valid: true},
	})
	if err != nil {
		return gateway.Key{}, "", fmt.Errorf("record gateway key: %w", err)
	}
	w.sb = sb
	r.log.Info("issued agentgateway key", append(w.logAttrs(), "key_id", issued.ID, "reason", reason)...)
	return issued.Key, issued.Material, nil
}

// ensureWorkspace makes sure the org's workspace exists, once per pass.
func (r *Reconciler) ensureWorkspace(ctx context.Context, org store.ListOrgsToReconcileRow, done map[string]string) (string, error) {
	id := uuidString(org.ID)
	if ws, ok := done[id]; ok {
		return ws, nil
	}
	name := WorkspaceName(org.ID)
	if row, err := r.q.GetOrgWorkspace(ctx, org.ID); err == nil {
		name = row.Workspace
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("reconciler: read workspace of org %s: %w", org.Slug, err)
	}
	ws, err := r.backend.EnsureWorkspace(ctx, name, ownerLabels(org.ID, pgtype.UUID{}))
	if err != nil {
		return "", fmt.Errorf("reconciler: ensure workspace of org %s: %w", org.Slug, err)
	}
	if _, err := r.q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{
		OrgID: org.ID, Workspace: name, WorkspaceID: ws.ID,
	}); err != nil {
		return "", fmt.Errorf("reconciler: record workspace of org %s: %w", org.Slug, err)
	}
	done[id] = name
	return name, nil
}

func (r *Reconciler) reconcileAgent(ctx context.Context, w *work, workspaces map[string]string) error {
	switch w.agent.Status {
	case store.AgentStatusActive:
		return r.reconcileActive(ctx, w, workspaces)
	case store.AgentStatusArchived:
		return r.reconcileArchived(ctx, w)
	default:
		return r.reconcileDraft(ctx, w)
	}
}

func (r *Reconciler) reconcileActive(ctx context.Context, w *work, workspaces map[string]string) error {
	if w.keyErr != nil {
		return w.keyErr
	}
	flavor, ok := r.cfg.Flavors[w.agent.Flavor]
	if !ok || flavor.Image == "" || len(flavor.Binaries) == 0 {
		return fmt.Errorf("flavor %q has no image or binaries configured", w.agent.Flavor)
	}
	ws, err := r.ensureWorkspace(ctx, w.org, workspaces)
	if err != nil {
		return err
	}
	names := Names{Sandbox: w.sb.SandboxName, Provider: w.sb.ProviderName, KeySecret: w.sb.KeySecretName}

	// The key goes only into the provider record; the sandbox sees a placeholder.
	if err := r.backend.EnsureProvider(ctx, ws, execution.Provider{
		Name:               names.Provider,
		Profile:            r.profile(w.agent.Flavor, flavor),
		Credentials:        map[string]string{keyEnvVar: w.material},
		CredentialRevision: w.key.ID,
		Labels:             ownerLabels(w.org.ID, w.agent.ID),
	}); err != nil {
		return err
	}

	spec, hash := r.sandboxSpec(w.org.ID, w.agent.ID, names, flavor)
	act, err := r.q.GetAgentRunActivity(ctx, store.GetAgentRunActivityParams{OrgID: w.org.ID, AgentID: w.agent.ID})
	if err != nil {
		return fmt.Errorf("read run activity: %w", err)
	}

	sb, err := r.backend.GetSandbox(ctx, ws, names.Sandbox)
	if errors.Is(err, execution.ErrNotFound) {
		sb, err = r.backend.CreateSandbox(ctx, ws, spec)
		if err != nil {
			return err
		}
		r.log.Info("created sandbox", w.logAttrs()...)
		return r.record(ctx, w, sb, hash, "")
	}
	if err != nil {
		return err
	}

	if sb.Phase == execution.PhaseDeleting {
		return r.record(ctx, w, sb, hash, "")
	}
	if sb.Labels[LabelSpecHash] != hash {
		// Image, providers or filesystem rules changed (or the sandbox was replaced by
		// hand): OpenShell cannot change those in place, so the sandbox is recreated, which
		// also replaces its home volume. Never while a run holds the agent.
		if act.HasActive {
			return r.record(ctx, w, sb, hash, "sandbox spec changed; recreating it after the active run")
		}
		if err := r.backend.DeleteSandbox(ctx, ws, names.Sandbox); err != nil {
			return err
		}
		r.log.Info("deleting sandbox to recreate it with a changed spec", w.logAttrs()...)
		sb.Phase = execution.PhaseDeleting
		return r.record(ctx, w, sb, hash, "recreating with a changed spec")
	}

	switch sb.Phase {
	case execution.PhaseReady:
		if _, err := r.backend.EnsurePolicy(ctx, ws, names.Sandbox, spec.Policy); err != nil {
			return err
		}
		if !act.HasActive && !act.HasDemand && r.idle(w.sb, act) {
			if sb, err = r.backend.StopSandbox(ctx, ws, names.Sandbox); err != nil {
				return err
			}
			r.log.Info("stopped idle sandbox", w.logAttrs()...)
		}
	case execution.PhaseStopped:
		if act.HasActive || act.HasDemand {
			if sb, err = r.backend.StartSandbox(ctx, ws, names.Sandbox); err != nil {
				return err
			}
			r.log.Info("started sandbox for a run", w.logAttrs()...)
		}
	}
	return r.record(ctx, w, sb, hash, "")
}

// idle reports whether the sandbox has had no run activity for IdleTimeout, counting from the
// later of its last start and the last run that finished or parked.
func (r *Reconciler) idle(sb store.AgentSandbox, act store.GetAgentRunActivityRow) bool {
	var since time.Time
	if sb.StartedAt.Valid {
		since = sb.StartedAt.Time
	}
	if act.LastActivity.Valid && act.LastActivity.Time.After(since) {
		since = act.LastActivity.Time
	}
	if since.IsZero() {
		// Ready but never recorded as ready: start counting now.
		return false
	}
	return r.cfg.Now().Sub(since) >= r.cfg.IdleTimeout
}

// reconcileDraft stops the sandbox of an agent returned to draft and keeps everything else
// for its re-activation. Runs cannot be queued for a draft agent.
func (r *Reconciler) reconcileDraft(ctx context.Context, w *work) error {
	sb, err := r.backend.GetSandbox(ctx, w.sb.Workspace, w.sb.SandboxName)
	if errors.Is(err, execution.ErrNotFound) {
		return r.recordState(ctx, w, store.SandboxStatePending, "", "")
	}
	if err != nil {
		return err
	}
	if sb.Phase == execution.PhaseReady {
		if sb, err = r.backend.StopSandbox(ctx, w.sb.Workspace, w.sb.SandboxName); err != nil {
			return err
		}
		r.log.Info("stopped sandbox of a draft agent", w.logAttrs()...)
	}
	return r.record(ctx, w, sb, w.sb.SpecHash, "")
}

// reconcileArchived deletes the agent's sandbox (or, with RetentionRetain, stops it), then its
// provider record and key Secret. The key itself was already revoked: the gateway state of
// this pass left it out.
func (r *Reconciler) reconcileArchived(ctx context.Context, w *work) error {
	ws, name := w.sb.Workspace, w.sb.SandboxName
	final := store.SandboxStateDeleted
	sb, err := r.backend.GetSandbox(ctx, ws, name)
	switch {
	case errors.Is(err, execution.ErrNotFound):
	case err != nil:
		return err
	case r.cfg.HomeRetention == RetentionRetain:
		if sb.Phase == execution.PhaseReady {
			if sb, err = r.backend.StopSandbox(ctx, ws, name); err != nil {
				return err
			}
		}
		if sb.Phase != execution.PhaseStopped {
			return r.record(ctx, w, sb, w.sb.SpecHash, "archived; waiting for the sandbox to stop")
		}
		final = store.SandboxStateRetained
	default:
		if sb.Phase != execution.PhaseDeleting {
			if err := r.backend.DeleteSandbox(ctx, ws, name); err != nil {
				return err
			}
			r.log.Info("deleting sandbox of an archived agent", w.logAttrs()...)
		}
		sb, err = r.backend.GetSandbox(ctx, ws, name)
		if err == nil {
			return r.recordState(ctx, w, store.SandboxStateDeleting, string(sb.Phase), "archived; waiting for the sandbox to be deleted")
		}
		if !errors.Is(err, execution.ErrNotFound) {
			return err
		}
	}

	if err := r.backend.DeleteProvider(ctx, ws, w.sb.ProviderName); err != nil {
		return err
	}
	if err := r.secrets.Delete(ctx, w.sb.KeySecretName); err != nil {
		return err
	}
	if _, err := r.q.ClearAgentSandboxKey(ctx, store.ClearAgentSandboxKeyParams{OrgID: w.org.ID, AgentID: w.agent.ID}); err != nil {
		return fmt.Errorf("clear gateway key: %w", err)
	}
	r.log.Info("cleaned up archived agent", append(w.logAttrs(), "home", string(r.cfg.HomeRetention))...)
	phase := ""
	if final == store.SandboxStateRetained {
		phase = string(execution.PhaseStopped)
	}
	return r.recordState(ctx, w, final, phase, "")
}

func (r *Reconciler) record(ctx context.Context, w *work, sb execution.Sandbox, hash, note string) error {
	msg := sb.Message
	if note != "" {
		msg = note
	}
	return r.recordState(ctx, w, sandboxState(sb.Phase), string(sb.Phase), msg, hash)
}

func (r *Reconciler) recordState(ctx context.Context, w *work, state store.SandboxState, phase, msg string, hash ...string) error {
	h := w.sb.SpecHash
	if len(hash) > 0 {
		h = hash[0]
	}
	sb, err := r.q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
		OrgID: w.org.ID, AgentID: w.agent.ID,
		State: state, Phase: phase, Message: msg, SpecHash: h,
	})
	if err != nil {
		return fmt.Errorf("record sandbox state: %w", err)
	}
	w.sb = sb
	return nil
}

// recordMessage stores a reconcile error on the row for operators, keeping the state.
func (r *Reconciler) recordMessage(ctx context.Context, w *work, err error) {
	if w.sb.SandboxName == "" {
		return
	}
	_ = r.recordState(ctx, w, w.sb.State, w.sb.Phase, err.Error())
}
