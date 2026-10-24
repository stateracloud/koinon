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
	"maps"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
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
	// DefaultMCPProbeInterval is how often each org MCP server is probed (plan item MC1).
	DefaultMCPProbeInterval = 60 * time.Second
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
	// Keys opens the provider keys stored in Postgres (the secret store).
	Keys catalog.Opener
	// ProviderKeys receives the opened keys for the gateway. Required when any served
	// provider has a key.
	ProviderKeys ProviderKeyStore
	// ProviderKeyDir is where the gateway mounts ProviderKeys, for the rendered config.
	ProviderKeyDir string
	// AgentLimit computes an agent's budget; nil is ledger.AgentLimit. It is a seam for tests.
	AgentLimit func(ctx context.Context, q *store.Queries, agent store.Agent, now time.Time) (*gateway.Limit, error)
	// URLPolicy is checked again, with a fresh lookup, for every provider URL before it is served.
	URLPolicy catalog.URLPolicy
	// ProviderKeySettle is how long a pass waits, after writing a new provider key file, before it
	// applies a config that names the file: kubelet needs about a minute to update a mounted Secret
	// and agentgateway rejects a config whose key file is missing. Zero applies at once.
	ProviderKeySettle time.Duration
	// ToolServers maps a connector type ID to the URL of its MCP tool server
	// (for github: koinon-api's tools listener, plan item C3). Each becomes an
	// agentgateway MCP target named after the connector, and every active
	// agent is allowed the tools its approved grants expose. Empty serves no
	// tools.
	ToolServers map[string]string
	// MCPAskURL is the base URL, as agentgateway reaches it, of koinon-api's approval-gated MCP tool
	// proxy (internal/mcpask); an org server "wiki" has its ask-tier tools at <MCPAskURL>/wiki. Empty
	// offers no ask-tier tools of org MCP servers.
	MCPAskURL string
	// MCPProbeInterval is the time between health probes of each org MCP server. Default
	// DefaultMCPProbeInterval.
	MCPProbeInterval time.Duration
	// DiagnoseMCP asks an MCP server directly why the gateway could not use it; default
	// mcpservers.Diagnose. A seam for tests.
	DiagnoseMCP func(ctx context.Context, policy catalog.URLPolicy, p mcpservers.PreflightRequest) mcpservers.Diagnosis
	// GitHub mints the installation tokens of repo.read grants (plan item C2). Nil leaves
	// agents with such a grant unconverged, with an error.
	GitHub GitHubTokens
	// GitBinaries may use the GitHub endpoints of a read grant, besides the flavor's binaries.
	// Default DefaultGitBinaries.
	GitBinaries []string
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

	mu sync.Mutex
	// githubRevs is the token revision last pushed per GitHub provider record, to log refreshes.
	githubRevs map[string]string
	// unpricedLogged is the unpriced model set last logged.
	unpricedLogged string
	// keyWrittenAt is when each provider key file written by this process was written, until it
	// has had ProviderKeySettle to reach the gateway.
	keyWrittenAt map[string]time.Time
	// lastLimit is the budget last pushed per agent key, so one agent whose budget cannot be
	// computed keeps it (and never gets more) instead of stopping the pass for everyone.
	lastLimit map[gateway.AgentRef]gateway.Limit
	// appliedFile is the key file (data key) each provider ID was last applied with.
	appliedFile map[string]string
	// appliedMCPFile is the same for org MCP servers, by server ID.
	appliedMCPFile map[string]string
	// probeKeys are the prober's gateway keys, in memory only; probes are the servers the last
	// applied state lets the prober list.
	probeKeys map[gateway.AgentRef]gateway.IssuedKey
	probes    []mcpProbe
	mcp       *mcpservers.Service
}

// New returns a Reconciler.
func New(pool *pgxpool.Pool, backend execution.Backend, gw gateway.ModelGateway, secrets SecretStore, cfg Config) (*Reconciler, error) {
	if cfg.Gateway.Host == "" || cfg.Gateway.Port == 0 {
		return nil, errors.New("reconciler: the agentgateway endpoint is required")
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.DiagnoseMCP == nil {
		cfg.DiagnoseMCP = mcpservers.Diagnose
	}
	if cfg.MCPProbeInterval == 0 {
		cfg.MCPProbeInterval = DefaultMCPProbeInterval
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
	if cfg.GitBinaries == nil {
		cfg.GitBinaries = DefaultGitBinaries
	}
	return &Reconciler{
		cfg: cfg, log: cfg.Logger, pool: pool, q: store.New(pool),
		backend: backend, gw: gw, secrets: secrets, githubRevs: map[string]string{},
		mcp: mcpservers.New(pool, nil, cfg.Logger),
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
	// cfg is the active version's config, for active agents.
	cfg agentconfig.Config
	// tools are the MCP tools the active version's grants allow.
	tools []gateway.ToolGrant
}

// names are the agent's object names: the recorded ones, and the GitHub ones derived from the
// IDs (they are not recorded: agentd never needs them).
func (w *work) names() execution.AgentNames {
	n := execution.AgentObjectNames(w.org.ID.Bytes, w.agent.ID.Bytes)
	n.Sandbox, n.Provider, n.KeySecret = w.sb.SandboxName, w.sb.ProviderName, w.sb.KeySecretName
	return n
}

func (w *work) logAttrs() []any {
	return []any{"org", w.org.Slug, "agent", execution.UUIDString(w.agent.ID.Bytes), "sandbox", w.sb.SandboxName}
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
		tools = map[gateway.AgentRef][]gateway.ToolGrant{}
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
			if len(w.tools) > 0 {
				tools[agentRef(org, a.ID)] = w.tools
			}
		}
	}

	// The org's providers and models (PV1). Without the list an Apply would stop serving every
	// model, so a database error aborts the pass.
	served, provErrs, err := r.loadProviders(ctx, orgs)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("%w: %w", errAbortPass, err))...)
	}
	errs = append(errs, provErrs...)
	// The orgs' remote MCP servers (MC1), likewise.
	mcpServed, mcpErrs, err := r.loadMCP(ctx, orgs)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("%w: %w", errAbortPass, err))...)
	}
	errs = append(errs, mcpErrs...)

	// One gateway state for every agent: keys of archived agents are revoked by omission.
	state := gateway.State{Keys: keys, Providers: served.providers, MCP: r.mcpTargets(), Tools: tools}
	r.warnUnpriced(state)
	// Spend limits net of recorded spend (plan item L1), recomputed every pass, so a key
	// rotation or a gateway that lost its budget counters gets the remainder, not the full
	// limit. An agent whose budget cannot be computed fails closed (its last pushed limit, or
	// zero) and is reported; it never stops the pass for the other agents and orgs.
	now := r.cfg.Now()
	for _, w := range items {
		if w.key.ID == "" {
			continue
		}
		limitFor := r.cfg.AgentLimit
		if limitFor == nil {
			limitFor = ledger.AgentLimit
		}
		l, err := limitFor(ctx, r.q, w.agent, now)
		if err != nil {
			failed := r.failClosedLimit(w.key.Agent, now)
			r.log.Error("agent budget not computed; its key keeps its last pushed limit, or zero if it had none",
				"agent", execution.UUIDString(w.agent.ID.Bytes), "org", w.key.Agent.Org, "err", err)
			errs = append(errs, fmt.Errorf("spend limit of agent %s not computed (key fails closed): %w", execution.UUIDString(w.agent.ID.Bytes), err))
			state.SetLimit(w.key.Agent, failed)
			continue
		}
		r.rememberLimit(w.key.Agent, l)
		state.SetLimit(w.key.Agent, l)
	}
	// Provider key files first, then the config that names them; files no provider uses go last.
	// One provider's new key file still travelling to the gateway pod never holds back the others:
	// that provider keeps serving with its previous file, or is left out until the file has arrived.
	var written []string
	files := map[string][]byte{}
	maps.Copy(files, served.files)
	maps.Copy(files, mcpServed.files)
	if len(files) > 0 {
		var err error
		if written, err = r.cfg.ProviderKeys.Put(ctx, files); err != nil {
			return errors.Join(append(errs, fmt.Errorf("reconciler: deliver provider keys: %w", err))...)
		}
	}
	settling := r.keysSettling(written, now)
	keep := r.settleProviders(&state, served, settling)
	maps.Copy(keep, r.settleMCP(&mcpServed, settling))
	probes, err := r.applyMCP(ctx, &state, &mcpServed, items)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if _, err := r.gw.Apply(ctx, state); err != nil {
		errs = append(errs, fmt.Errorf("reconciler: apply gateway state: %w", err))
		return errors.Join(errs...)
	}
	r.recordApplied(served, state)
	r.recordAppliedMCP(mcpServed, probes)
	if r.cfg.ProviderKeys != nil && len(provErrs) == 0 && len(mcpErrs) == 0 {
		if err := r.cfg.ProviderKeys.Prune(ctx, keep); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: prune provider keys: %w", err))
		}
	}

	for _, w := range items {
		if err := r.reconcileAgent(ctx, w, workspaces); err != nil {
			r.log.Warn("agent not converged", append(w.logAttrs(), "err", err)...)
			r.recordMessage(ctx, w, err)
			errs = append(errs, fmt.Errorf("agent %s: %w", execution.UUIDString(w.agent.ID.Bytes), err))
		}
	}
	return errors.Join(errs...)
}

// settleProviders handles providers whose key file is still settling: with a previously applied
// file the provider keeps using it, without one it is left out of state. It returns the data keys to
// keep in the provider keys Secret.
func (r *Reconciler) settleProviders(state *gateway.State, served servedProviders, settling map[string]bool) map[string]bool {
	keep := map[string]bool{}
	for name := range served.files {
		keep[name] = true
	}
	if len(settling) == 0 {
		return keep
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []gateway.Provider
	for i, p := range served.providers {
		name := served.fileNames[i]
		if name == "" || !settling[name] {
			out = append(out, p)
			continue
		}
		if prev := r.appliedFile[served.ids[i]]; prev != "" {
			p.APIKeyFile = path.Join(r.cfg.ProviderKeyDir, prev)
			keep[prev] = true
			out = append(out, p)
			r.log.Info("a new provider key is still reaching the gateway; serving with the previous one", "org", p.Org, "provider", p.Name)
			continue
		}
		r.log.Info("a new provider key is still reaching the gateway; the provider is not served until it has", "org", p.Org, "provider", p.Name)
	}
	state.Providers = out
	return keep
}

// recordApplied remembers which key file each served provider was applied with.
func (r *Reconciler) recordApplied(served servedProviders, state gateway.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]string{}
	for i, id := range served.ids {
		for _, p := range state.Providers {
			if p.Org == served.providers[i].Org && p.Name == served.providers[i].Name && p.APIKeyFile != "" {
				next[id] = path.Base(p.APIKeyFile)
			}
		}
	}
	r.appliedFile = next
}

// keysSettling records the files written in this pass and returns the data keys that have not yet
// had ProviderKeySettle to reach the gateway pod.
func (r *Reconciler) keysSettling(written []string, now time.Time) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keyWrittenAt == nil {
		r.keyWrittenAt = map[string]time.Time{}
	}
	for _, name := range written {
		r.keyWrittenAt[name] = now
	}
	settling := map[string]bool{}
	for name, at := range r.keyWrittenAt {
		if r.cfg.ProviderKeySettle-now.Sub(at) > 0 {
			settling[name] = true
		} else {
			delete(r.keyWrittenAt, name)
		}
	}
	return settling
}

// warnUnpriced logs the models the gateway does not serve for lack of a price, once per set.
func (r *Reconciler) warnUnpriced(s gateway.State) {
	unpriced := s.Unpriced()
	r.mu.Lock()
	defer r.mu.Unlock()
	key := fmt.Sprint(unpriced)
	if len(unpriced) > 0 && key != r.unpricedLogged {
		r.log.Warn("models without a price are not served; give free models a zero price", "models", unpriced)
	}
	r.unpricedLogged = key
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
		names := execution.AgentObjectNames(org.ID.Bytes, a.ID.Bytes)
		w.sb, err = r.q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
			OrgID: org.ID, AgentID: a.ID, Workspace: ws,
			SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
		})
		if err != nil {
			return nil, fmt.Errorf("reconciler: record sandbox of agent %s: %w", execution.UUIDString(a.ID.Bytes), err)
		}
	default:
		// Without the row, the agent's key would be left out of the gateway state and so
		// revoked: abort the pass instead.
		return nil, fmt.Errorf("%w: read sandbox of agent %s: %w", errAbortPass, execution.UUIDString(a.ID.Bytes), err)
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
		cfg, err := r.activeConfig(ctx, a)
		if err != nil {
			// Without it the agent's tools would be revoked by omission.
			return nil, fmt.Errorf("%w: %w", errAbortPass, err)
		}
		w.cfg = cfg
		if len(r.cfg.ToolServers) > 0 {
			w.tools = ToolGrants(cfg, r.cfg.ToolServers)
		}
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
	return gateway.AgentRef{Org: org.Slug, Name: execution.UUIDString(agent.Bytes)}
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
	id := execution.UUIDString(org.ID.Bytes)
	if ws, ok := done[id]; ok {
		return ws, nil
	}
	name := execution.WorkspaceName(org.ID.Bytes)
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
	names := w.names()

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

	// The GitHub read grant: its provider record before the sandbox that attaches it.
	read, hasRead, err := GitHubReadAccess(w.cfg)
	if err != nil {
		return err
	}
	providers := []string{names.Provider}
	network := map[string]execution.NetworkRule{}
	if hasRead {
		if err := r.ensureGitHub(ctx, ws, w, names, flavor, read); err != nil {
			return err
		}
		providers = append(providers, names.GitHubProvider)
		network[githubRule] = GitHubRule(read, r.githubBinaries(flavor))
	}
	if rule, ok := PluginInstallRule(w.cfg, w.agent.Flavor, flavor); ok {
		network[npmRule] = rule
	}

	spec, hash := r.sandboxSpec(w.org.ID, w.agent.ID, names, flavor, providers, network)
	act, err := r.q.GetAgentRunActivity(ctx, store.GetAgentRunActivityParams{OrgID: w.org.ID, AgentID: w.agent.ID})
	if err != nil {
		return fmt.Errorf("read run activity: %w", err)
	}

	sb, err := r.backend.GetSandbox(ctx, ws, names.Sandbox)
	if errors.Is(err, execution.ErrNotFound) {
		if !hasRead {
			// No sandbox references a GitHub record now: drop one left by a removed grant.
			if err := r.deleteGitHub(ctx, ws, names); err != nil {
				return err
			}
		}
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
		// also replaces its home volume. Never while a run holds the agent, parked runs
		// included: a parked run resumes its harness session, which lives in that volume.
		// (agentd starts a new session when the volume was replaced anyway, see
		// agent_sandboxes.home_generation.)
		if act.HasHeld {
			return r.record(ctx, w, sb, hash, "sandbox spec changed; recreating it after the active and parked runs finish")
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
	if err := r.deleteGitHub(ctx, ws, w.names()); err != nil {
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

// record stores the observed sandbox. Its OpenShell ID is the home generation: OpenShell
// deletes the home volume with the sandbox, so a new sandbox (a new ID) means a new, empty home,
// while stop and start keep both.
func (r *Reconciler) record(ctx context.Context, w *work, sb execution.Sandbox, hash, note string) error {
	msg := sb.Message
	if note != "" {
		msg = note
	}
	return r.recordRow(ctx, w, sandboxState(sb.Phase), string(sb.Phase), msg, hash, sb.ID)
}

// recordState stores a state without an observed sandbox; the recorded home generation is kept.
func (r *Reconciler) recordState(ctx context.Context, w *work, state store.SandboxState, phase, msg string, hash ...string) error {
	h := w.sb.SpecHash
	if len(hash) > 0 {
		h = hash[0]
	}
	return r.recordRow(ctx, w, state, phase, msg, h, "")
}

func (r *Reconciler) recordRow(ctx context.Context, w *work, state store.SandboxState, phase, msg, hash, generation string) error {
	sb, err := r.q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
		OrgID: w.org.ID, AgentID: w.agent.ID,
		State: state, Phase: phase, Message: msg, SpecHash: hash, HomeGeneration: generation,
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

// rememberLimit records the budget pushed for agent.
func (r *Reconciler) rememberLimit(agent gateway.AgentRef, l *gateway.Limit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastLimit == nil {
		r.lastLimit = map[gateway.AgentRef]gateway.Limit{}
	}
	if l == nil {
		delete(r.lastLimit, agent)
		return
	}
	r.lastLimit[agent] = *l
}

// failClosedLimit is the budget for an agent whose budget could not be computed: the last one
// pushed while it is still from the current UTC day, else zero. A pushed limit only ever shrinks
// as the gateway charges against it, but it must not be regranted every day by a computation that
// keeps failing; zero blocks the key until a pass computes it.
func (r *Reconciler) failClosedLimit(agent gateway.AgentRef, now time.Time) *gateway.Limit {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.lastLimit[agent]; ok {
		day, _, _ := strings.Cut(l.Generation, "-")
		if day == strconv.FormatInt(budget.DayStart(now).Unix(), 10) {
			return &l
		}
	}
	return &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: ledger.Window, Generation: "failed"}
}
