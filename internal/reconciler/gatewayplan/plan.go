// SPDX-License-Identifier: Apache-2.0

// Package gatewayplan decides the gateway side of a reconcile pass (old issue 455, ADR 0058).
//
// The reconciler loads rows from Postgres and runs the steps; this package decides what the gateway
// is told. Given the loaded rows and the time, a Planner returns the state to apply, the key files
// to write first, the budget stops to issue, the key files to keep, and which agents' sandbox jobs
// may start now and which must wait for the Apply. The result of the Apply is fed back, so the
// Planner learns which keys are live and which files were applied. Nothing here depends on the
// gateway mode (standalone or Kubernetes), and nothing talks to Postgres or a gateway.
//
// The rules it keeps, each with a table in plan_test.go:
//
//   - a key file is written before the config that names it (Draft.Files, then Settle);
//   - an omitted key means revoked (the state carries exactly the keys of the pass);
//   - a limit that cannot be computed fails closed (the last limit of the day, else zero);
//   - a budget stop comes before the revoke (Draft.Stops are issued before the Apply);
//   - one rejected key fails alone (Outcome.Run and Held are per agent);
//   - after a restart everything is held back until something is applied (Planner.KeyNotLive).
//
// The memory these rules need between passes lives in the Planner, not in the Reconciler.
//
// A pass has three steps: Begin (the state, the limits, the stops, the files), Draft.Settle (once the
// files are written: what is served, the MCP and model-check keys, who starts early) and Plan.Applied
// (after the Apply). A few reads depend on what the plan itself decided (the key a system principal
// holds, the key file a provider was last served with); they go through Lookups.
package gatewayplan

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// Org is an org of the pass.
type Org struct {
	ID   pgtype.UUID
	Slug string
}

// Agent is one agent of the pass, as the reconciler prepared it.
type Agent struct {
	Org Org
	// ID is the agent's ID, for its MCP allowances.
	ID pgtype.UUID
	// Ref is the agent's name at the gateway; Tools are keyed by it.
	Ref gateway.AgentRef
	// Active is set for an active agent.
	Active bool
	// Key is the agent's key, zero (empty ID) when the agent has none in hand.
	Key gateway.Key
	// Tools are the MCP tools the active version's grants allow.
	Tools []gateway.ToolGrant
	// Limit is the agent's budget net of spend; LimitErr is set instead when it could not be computed.
	// Only agents with a key are looked at.
	Limit    *gateway.Limit
	LimitErr error
}

// Provider is a provider the pass serves.
type Provider struct {
	// ID is the provider's UUID string.
	ID                string
	OrgID, ProviderID pgtype.UUID
	// File is the key file's data key in the provider keys Secret, "" for a keyless provider.
	File    string
	Gateway gateway.Provider
}

// Tier is the approval tier of an MCP tool.
type Tier int

// Tiers.
const (
	TierNever Tier = iota
	TierAuto
	TierAsk
)

// MCPTool is one tool of an org MCP server.
type MCPTool struct {
	Name    string
	Present bool
	Tier    Tier
}

// MCPServer is an org MCP server the pass serves.
type MCPServer struct {
	Org  Org
	ID   pgtype.UUID
	Name string
	URL  string
	SSE  bool
	// CredentialHeader and CredentialPrefix describe how the credential is sent, when File is set.
	CredentialHeader string
	CredentialPrefix string
	// Unhealthy servers give no tools to any agent.
	Unhealthy bool
	// File is the credential's data key in the provider keys Secret, "" for a server without one.
	File  string
	Tools []MCPTool
}

// Allowed is one agent's allowance of an MCP server.
type Allowed struct {
	Server pgtype.UUID
	// Tools narrows the allowance to these tools; nil allows all of the server's.
	Tools []string
}

// Input is everything one pass loaded.
type Input struct {
	Now    time.Time
	Orgs   []Org
	Agents []Agent
	// Providers, their key files (by data key) and the ID of each rendered model (gateway.ModelRef).
	Providers     []Provider
	ProviderFiles map[string][]byte
	ModelIDs      map[string]pgtype.UUID
	// MCP are the orgs' servers and their credential files.
	MCP      []MCPServer
	MCPFiles map[string][]byte
	// LedgerEnforcesBudgets is set when the gateway cannot enforce a limit itself (Kubernetes mode:
	// the ledger revokes the key at zero), so a limit that fails closed is zero.
	LedgerEnforcesBudgets bool
	// Allowances are the active agents' MCP allowlists, by org slug and agent ID. The caller loads them
	// before Begin when the pass serves MCP servers, so nothing can fail between writing the key files
	// and Settle recording their write time.
	Allowances map[string]map[pgtype.UUID][]Allowed
}

// Lookups are the reads that depend on what the plan decided. A nil Lookups is not allowed where
// the plan needs one.
type Lookups interface {
	// SystemKey returns the org's key for a system principal of Koinon's own (the MCP executor, the
	// model check), issuing it on first use and keeping it in the Secret secret.
	SystemKey(ctx context.Context, secret string, org Org, ref gateway.AgentRef) (gateway.Key, error)
	// IssueKey issues a key for ref (the MCP prober's).
	IssueKey(ctx context.Context, ref gateway.AgentRef) (gateway.IssuedKey, error)
	// SystemLimit is the org's model-check budget net of spend.
	SystemLimit(ctx context.Context, org Org, now time.Time) (*gateway.Limit, error)
	// ServedKeyFile is the key file the provider was last recorded as served with; found is false
	// when there is no record.
	ServedKeyFile(ctx context.Context, org, provider pgtype.UUID) (file string, found bool, err error)
}

// Config configures a Planner.
type Config struct {
	// KeyDir is where the gateway mounts the provider key files.
	KeyDir string
	// KeySettle is how long a new key file needs to reach the gateway pod.
	KeySettle time.Duration
	// Wake, when set, is called once KeySettle after a pass wrote new key files: the exact end of the
	// settle, so the next pass serves the provider or MCP server at once instead of at the next resync
	// (#249). It runs on its own goroutine and must not block for long.
	Wake func()
	// ToolServers maps a connector type ID to its MCP tool server URL.
	ToolServers map[string]string
	// MCPAskURL is the base URL of the approval-gated MCP tool proxy; empty offers no ask-tier tools.
	MCPAskURL string
	Logger    *slog.Logger
}

// Probe is one MCP server the prober lists once the state naming it is applied.
type Probe struct {
	Org      pgtype.UUID
	OrgSlug  string
	Server   pgtype.UUID
	Name     string
	Target   string
	Material string
}

// ProblemKind says what a Problem is about, for the pass metrics.
type ProblemKind int

// Problem kinds.
const (
	ProblemProvider ProblemKind = iota
	ProblemSpendLimit
)

// Problem is something the pass reports without stopping.
type Problem struct {
	Kind ProblemKind
	Err  error
}

// LimitFailure is an agent whose budget could not be computed. Its key was given the fail-closed
// limit.
type LimitFailure struct {
	// Agent indexes Input.Agents.
	Agent int
	Err   error
}

// Planner is the gateway memory between passes.
type Planner struct {
	cfg Config
	log *slog.Logger

	mu sync.Mutex
	// keyWrittenAt is when each key file written by this process was written, until it has had
	// KeySettle to reach the gateway.
	keyWrittenAt map[string]time.Time
	// wakeTimers are the pending settle-end wakes; closed (set by Close) stops them and any new one.
	wakeTimers map[*time.Timer]struct{}
	closed     bool
	// lastLimit is the budget last pushed per agent key, so one agent whose budget cannot be computed
	// keeps it (and never gets more) instead of stopping the pass for everyone.
	lastLimit map[gateway.AgentRef]gateway.Limit
	// notLive holds the IDs of keys issued by this process that no applied state has carried yet.
	notLive map[string]bool
	// appliedSinceStart is set once a state has been applied by this process. Before that, notLive is
	// empty after a restart whatever the gateway serves, so a failing Apply holds back every key.
	appliedSinceStart bool
	// appliedFile is the key file each provider ID was last applied with; appliedMCPFile the same for
	// MCP servers.
	appliedFile    map[string]string
	appliedMCPFile map[string]string
	// appliedMCPSig is what each served MCP server was last applied as (its URL, transport and
	// credential file); a server whose signature is new or changed is probed at once (old issue 226).
	appliedMCPSig map[string]string
	// probeKeys are the prober's keys, in memory only; probes are the servers the last applied state
	// lets the prober list.
	probeKeys map[gateway.AgentRef]gateway.IssuedKey
	probes    []Probe
	// unpricedLogged is the unpriced model set last logged.
	unpricedLogged string
}

// New returns a Planner.
func New(cfg Config) *Planner {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Planner{cfg: cfg, log: cfg.Logger}
}

// Close stops the pending settle-end wake timers and keeps later passes from scheduling more. The
// reconciler calls it when Run ends, so no timer fires into a pool that is closing.
func (p *Planner) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for t := range p.wakeTimers {
		t.Stop()
	}
	p.wakeTimers = nil
}

// KeyIssued records that this process issued key id: it is not live until an applied state carries it.
func (p *Planner) KeyIssued(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notLive == nil {
		p.notLive = map[string]bool{}
	}
	p.notLive[id] = true
}

// KeyNotLive reports whether the gateway may not serve key id yet. Until this process has applied a
// state, it cannot tell, so every key counts as not live.
func (p *Planner) KeyNotLive(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.appliedSinceStart || p.notLive[id]
}

// Probes returns the servers the prober lists.
func (p *Planner) Probes() []Probe {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.probes)
}

// Draft is a pass before its key files are written.
type Draft struct {
	p     *Planner
	in    Input
	state gateway.State
	// Files are the key files to write first, by data key (providers' and MCP servers').
	Files map[string][]byte
	// Stops indexes the agents whose budget has nothing left: their runs are stopped with the budget
	// reason before the Apply blocks or revokes the key (old issue 188).
	Stops []int
	// LimitFailures are the agents whose budget could not be computed.
	LimitFailures []LimitFailure
}

// Begin builds the state for one pass: every agent's key and tools, the providers, the tool servers
// and the limits. An omitted key means revoked, so the state carries exactly in.Agents' keys.
func (p *Planner) Begin(in Input) *Draft {
	d := &Draft{p: p, in: in, Files: map[string][]byte{}}
	tools := map[gateway.AgentRef][]gateway.ToolGrant{}
	var keys []gateway.Key
	for _, a := range in.Agents {
		if a.Key.ID != "" {
			keys = append(keys, a.Key)
		}
		if len(a.Tools) > 0 {
			tools[a.Ref] = a.Tools
		}
	}
	providers := make([]gateway.Provider, 0, len(in.Providers))
	for _, sp := range in.Providers {
		providers = append(providers, sp.Gateway)
	}
	if len(in.Providers) == 0 {
		providers = nil
	}
	d.state = gateway.State{Keys: keys, Providers: providers, MCP: p.toolTargets(), Tools: tools}
	p.warnUnpriced(d.state)
	// Spend limits net of recorded spend (plan item L1). An agent whose budget cannot be computed
	// fails closed (its last pushed limit, or zero) and is reported; it never stops the pass for the
	// other agents and orgs.
	for i, a := range in.Agents {
		if a.Key.ID == "" {
			continue
		}
		if a.LimitErr != nil {
			d.LimitFailures = append(d.LimitFailures, LimitFailure{Agent: i, Err: a.LimitErr})
			d.state.SetLimit(a.Key.Agent, p.failClosedLimit(a.Key.Agent, in.Now, in.LedgerEnforcesBudgets))
			continue
		}
		p.rememberLimit(a.Key.Agent, a.Limit)
		d.state.SetLimit(a.Key.Agent, a.Limit)
		// A limit that could not be computed fails closed above but stops nothing: it is not known to
		// be spent.
		if a.Limit != nil && a.Limit.Amount <= 0 {
			d.Stops = append(d.Stops, i)
		}
	}
	maps.Copy(d.Files, in.ProviderFiles)
	maps.Copy(d.Files, in.MCPFiles)
	return d
}

// Plan is a settled pass: what to apply and who may start when.
type Plan struct {
	p  *Planner
	in Input
	// State is what the gateway is told.
	State gateway.State
	// Keep are the key files that stay in the provider keys Secret after the Apply; the rest is pruned.
	Keep map[string]bool
	// Early indexes the agents whose sandbox work does not depend on the Apply; Late those with a key
	// the gateway may not serve yet, which wait for it.
	Early, Late []int
	// Problems are reported without stopping the pass.
	Problems   []Problem
	providers  []Provider
	servers    []*MCPServer
	probes     []Probe
	allowances map[string]map[pgtype.UUID][]Allowed
}

// Settle is the second step, run once the files are written (written and present are the key store's
// answer; both nil when there was nothing to write). Providers and MCP servers whose new file is
// still reaching the gateway pod keep their previous file or are left out; the MCP targets, grants
// and keys and the model-check keys are added. An error means the pass must stop before the Apply
// (an MCP key could not be had).
func (d *Draft) Settle(ctx context.Context, written []string, present map[string]bool, lk Lookups) (*Plan, error) {
	p, in := d.p, d.in
	pl := &Plan{p: p, in: in, State: d.state, allowances: in.Allowances}
	settling := p.keysSettling(written, in.Now)
	recorded := d.recordedFiles(ctx, settling, present, lk)
	pl.providers = slices.Clone(in.Providers)
	pl.Keep = p.settleProviders(&pl.State, pl, settling, recorded)
	servers := make([]*MCPServer, len(in.MCP))
	for i := range in.MCP {
		s := in.MCP[i]
		servers[i] = &s
	}
	var keepMCP map[string]bool
	servers, keepMCP = p.settleMCP(servers, in.MCPFiles, settling)
	pl.servers = servers
	maps.Copy(pl.Keep, keepMCP)
	probes, err := pl.applyMCP(ctx, lk)
	if err != nil {
		return nil, err
	}
	pl.probes = probes
	pl.applyModelCheck(ctx, lk)
	// An agent whose key the gateway already serves does not depend on the Apply, which may wait for
	// agentgateway's hot reload for about a minute: its sandbox starts and stops now. An agent with a
	// new key waits for the Apply.
	for i, a := range in.Agents {
		if a.Active && a.Key.ID != "" && p.KeyNotLive(a.Key.ID) {
			pl.Late = append(pl.Late, i)
			continue
		}
		pl.Early = append(pl.Early, i)
	}
	return pl, nil
}

// ApplyResult is what the Apply did.
type ApplyResult struct {
	// Failed is set when nothing was applied: the gateway was unreachable, the reload did not complete
	// or it rejected the whole config.
	Failed bool
	// Rejections are the objects a Kubernetes-mode gateway refused one by one; the rest is live.
	Rejections []gateway.Rejection
}

// ServedModels is what gateway_served_models should hold for one org after the Apply.
type ServedModels struct {
	Org Org
	// IDs and Files are parallel: each served model and the key file it is served with.
	IDs   []pgtype.UUID
	Files []string
	// PruneOnly is set for an org whose model objects the gateway rejected: nothing new is recorded,
	// but models no longer in the state are removed.
	PruneOnly bool
}

// Outcome is what follows an Apply.
type Outcome struct {
	// Run and Held split Plan.Late: the agents whose key is live now, and those that wait.
	Run, Held []int
	// Served is set when the Apply took effect: the models to record, one entry per org.
	Served []ServedModels
	// Probe is set when the Apply took effect: the MCP servers it newly serves or serves with another
	// URL, transport or credential. The prober lists them now instead of at its next tick.
	Probe []Probe
}

// Applied feeds the Apply's result back: the keys it carried become live, the key files and probes it
// applied are remembered, and the late agents are split into those that may start and those that wait.
func (pl *Plan) Applied(res ApplyResult) Outcome {
	var out Outcome
	if !res.Failed {
		pl.p.keysLive(pl.State, res.Rejections)
		pl.p.recordApplied(pl.providers, pl.State, res.Rejections)
		out.Served = pl.served(res.Rejections)
		out.Probe = pl.p.recordAppliedMCP(pl.servers, pl.probes)
	}
	for _, i := range pl.Late {
		if res.Failed || pl.p.KeyNotLive(pl.in.Agents[i].Key.ID) {
			out.Held = append(out.Held, i)
			continue
		}
		out.Run = append(out.Run, i)
	}
	return out
}

// served computes, for every org, the models the applied state serves, which koinon-agentd waits on
// before a run starts its harness (old issue 174). A provider whose new key file is still reaching the
// gateway pod and that has no previous file is not in the state, so its models are not recorded; one
// that keeps serving with its previous file stays recorded.
func (pl *Plan) served(rejections []gateway.Rejection) []ServedModels {
	rejected := rejectedModelOrgs(rejections)
	type entries struct {
		ids   []pgtype.UUID
		files []string
	}
	byOrg := map[string]*entries{}
	for _, p := range pl.State.Providers {
		e := byOrg[p.Org]
		if e == nil {
			e = &entries{}
			byOrg[p.Org] = e
		}
		file := ""
		if p.APIKeyFile != "" {
			file = path.Base(p.APIKeyFile)
		}
		for _, m := range p.Models {
			if m.Price == nil {
				continue // not served (ADR 0041)
			}
			if id, ok := pl.in.ModelIDs[gateway.ModelRef(p.Org, m.Name)]; ok {
				e.ids = append(e.ids, id)
				e.files = append(e.files, file)
			}
		}
	}
	out := make([]ServedModels, 0, len(pl.in.Orgs))
	for _, org := range pl.in.Orgs {
		// Never nil: pgx sends a nil slice as NULL, and "<> ALL (NULL)" would remove nothing.
		s := ServedModels{Org: org, IDs: []pgtype.UUID{}, Files: []string{}, PruneOnly: rejected[org.Slug]}
		if got := byOrg[org.Slug]; got != nil {
			s.IDs, s.Files = append(s.IDs, got.ids...), append(s.Files, got.files...)
		}
		out = append(out, s)
	}
	return out
}

func rejectedModelOrgs(rejections []gateway.Rejection) map[string]bool {
	rejected := map[string]bool{}
	for _, x := range rejections {
		if x.Kind == "AgentgatewayModel" {
			rejected[x.Org] = true
		}
	}
	return rejected
}

// toolTargets renders the configured tool servers as agentgateway MCP targets, sorted by name.
func (p *Planner) toolTargets() []gateway.MCPTarget {
	var out []gateway.MCPTarget
	for name, url := range p.cfg.ToolServers {
		out = append(out, gateway.MCPTarget{Name: name, URL: url})
	}
	slices.SortFunc(out, func(a, b gateway.MCPTarget) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// warnUnpriced logs the models the gateway does not serve for lack of a price, once per set.
func (p *Planner) warnUnpriced(s gateway.State) {
	unpriced := s.Unpriced()
	p.mu.Lock()
	defer p.mu.Unlock()
	key := fmt.Sprint(unpriced)
	if len(unpriced) > 0 && key != p.unpricedLogged {
		p.log.Warn("models without a price are not served; give free models a zero price", "models", unpriced)
	}
	p.unpricedLogged = key
}

// rememberLimit records the budget pushed for agent.
func (p *Planner) rememberLimit(agent gateway.AgentRef, l *gateway.Limit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastLimit == nil {
		p.lastLimit = map[gateway.AgentRef]gateway.Limit{}
	}
	if l == nil {
		delete(p.lastLimit, agent)
		return
	}
	p.lastLimit[agent] = *l
}

func failedLimit() *gateway.Limit {
	return &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: ledger.Window, Generation: "failed"}
}

// failClosedLimit is the budget for an agent whose budget could not be computed: the last one pushed
// while it is still from the current UTC day, else zero. A pushed limit only ever shrinks as the
// gateway charges against it, but it must not be regranted every day by a computation that keeps
// failing; zero blocks the key until a pass computes it.
//
// A gateway that does not enforce limits itself (Kubernetes mode: the ledger revokes the key when the
// amount is zero) cannot make a positive last limit bind, so there the key fails closed to zero.
func (p *Planner) failClosedLimit(agent gateway.AgentRef, now time.Time, ledgerEnforces bool) *gateway.Limit {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ledgerEnforces {
		return failedLimit()
	}
	if l, ok := p.lastLimit[agent]; ok {
		day, _, _ := strings.Cut(l.Generation, "-")
		if day == strconv.FormatInt(budget.DayStart(now).Unix(), 10) {
			return &l
		}
	}
	return failedLimit()
}

// keysSettling records the files written in this pass and returns the data keys that have not yet had
// KeySettle to reach the gateway pod.
func (p *Planner) keysSettling(written []string, now time.Time) map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keyWrittenAt == nil {
		p.keyWrittenAt = map[string]time.Time{}
	}
	for _, name := range written {
		p.keyWrittenAt[name] = now
	}
	if len(written) > 0 && p.cfg.Wake != nil && p.cfg.KeySettle > 0 {
		// One timer per pass. The settle ends when KeySettle has passed since now, and the timer starts
		// after now, so the settle is over when it fires.
		if !p.closed {
			if p.wakeTimers == nil {
				p.wakeTimers = map[*time.Timer]struct{}{}
			}
			var t *time.Timer
			t = time.AfterFunc(p.cfg.KeySettle, func() {
				p.mu.Lock()
				delete(p.wakeTimers, t)
				closed := p.closed
				p.mu.Unlock()
				if !closed {
					p.cfg.Wake()
				}
			})
			p.wakeTimers[t] = struct{}{}
		}
	}
	settling := map[string]bool{}
	for name, at := range p.keyWrittenAt {
		if p.cfg.KeySettle-now.Sub(at) > 0 {
			settling[name] = true
		} else {
			delete(p.keyWrittenAt, name)
		}
	}
	return settling
}

// recordedFiles returns, for each provider whose key file is settling and that this process has not
// applied, the key file gateway_served_models says it was last served with, when that file is still in
// the provider keys Secret: a key rotated while the reconciler was down (or before a new leader took
// over) keeps being served with the previous file instead of the provider being dropped. A failed read
// only loses the fallback.
func (d *Draft) recordedFiles(ctx context.Context, settling, present map[string]bool, lk Lookups) map[string]string {
	out := map[string]string{}
	if len(settling) == 0 {
		return out
	}
	for _, sp := range d.in.Providers {
		if sp.File == "" || !settling[sp.File] {
			continue
		}
		d.p.mu.Lock()
		applied := d.p.appliedFile[sp.ID]
		d.p.mu.Unlock()
		if applied != "" {
			continue
		}
		file, found, err := lk.ServedKeyFile(ctx, sp.OrgID, sp.ProviderID)
		switch {
		case err != nil:
			d.p.log.Warn("reading the key file a provider was last served with failed", "provider", sp.ID, "err", err)
		case found && file != sp.File && present[file]:
			out[sp.ID] = file
		}
	}
	return out
}

// settleProviders handles providers whose key file is still settling: with a previously applied file
// (this process's, else the recorded one) the provider keeps using it, without one it is left out of
// state. It returns the data keys to keep in the provider keys Secret.
func (p *Planner) settleProviders(state *gateway.State, pl *Plan, settling map[string]bool, recorded map[string]string) map[string]bool {
	keep := map[string]bool{}
	for name := range pl.in.ProviderFiles {
		keep[name] = true
	}
	if len(settling) == 0 {
		return keep
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []gateway.Provider
	for _, sp := range pl.in.Providers {
		prov := sp.Gateway
		if sp.File == "" || !settling[sp.File] {
			out = append(out, prov)
			continue
		}
		if prev := cmp.Or(p.appliedFile[sp.ID], recorded[sp.ID]); prev != "" {
			prov.APIKeyFile = path.Join(p.cfg.KeyDir, prev)
			keep[prev] = true
			out = append(out, prov)
			p.log.Info("a new provider key is still reaching the gateway; serving with the previous one", "org", prov.Org, "provider", prov.Name)
			continue
		}
		p.log.Info("a new provider key is still reaching the gateway; the provider is not served until it has", "org", prov.Org, "provider", prov.Name)
	}
	state.Providers = out
	return keep
}

// settleMCP leaves out servers whose new credential file is still reaching the gateway pod and have no
// previously applied one; with one, the server keeps using it. It returns the data keys to keep.
func (p *Planner) settleMCP(servers []*MCPServer, files map[string][]byte, settling map[string]bool) ([]*MCPServer, map[string]bool) {
	keep := map[string]bool{}
	for name := range files {
		keep[name] = true
	}
	if len(settling) == 0 {
		return servers, keep
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*MCPServer
	for _, s := range servers {
		if s.File == "" || !settling[s.File] {
			out = append(out, s)
			continue
		}
		if prev := p.appliedMCPFile[uuidKey(s.ID)]; prev != "" {
			s.File = prev
			keep[prev] = true
			out = append(out, s)
			p.log.Info("a new MCP server credential is still reaching the gateway; serving with the previous one", "org", s.Org.Slug, "server", s.Name)
			continue
		}
		p.log.Info("a new MCP server credential is still reaching the gateway; the server is not served until it has", "org", s.Org.Slug, "server", s.Name)
	}
	return out, keep
}

// keysLive records that the applied state carries its keys.
func (p *Planner) keysLive(s gateway.State, rejected []gateway.Rejection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appliedSinceStart = true
	refused := map[string]bool{}
	for _, x := range rejected {
		if x.Kind == "Key" {
			refused[x.Name] = true
		}
	}
	for _, k := range s.Keys {
		if !refused[k.ID] {
			delete(p.notLive, k.ID)
		}
	}
}

// recordApplied remembers which key file each served provider was applied with. A provider of an org
// whose model object the gateway rejected was not applied, so its file is not recorded.
func (p *Planner) recordApplied(providers []Provider, state gateway.State, rejections []gateway.Rejection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rejected := rejectedModelOrgs(rejections)
	next := map[string]string{}
	for _, sp := range providers {
		if rejected[sp.Gateway.Org] {
			continue
		}
		for _, pr := range state.Providers {
			if pr.Org == sp.Gateway.Org && pr.Name == sp.Gateway.Name && pr.APIKeyFile != "" {
				next[sp.ID] = path.Base(pr.APIKeyFile)
			}
		}
	}
	p.appliedFile = next
}

// recordAppliedMCP remembers the credential file each served server was applied with and the probes
// the applied state makes possible. It returns the probes of the servers whose applied signature is
// new or changed since the previous applied state.
func (p *Planner) recordAppliedMCP(servers []*MCPServer, probes []Probe) []Probe {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := map[string]string{}
	sigs := map[string]string{}
	changed := map[string]bool{}
	for _, s := range servers {
		k := uuidKey(s.ID)
		if s.File != "" {
			next[k] = s.File
		}
		sigs[k] = fmt.Sprintf("%s\x00%t\x00%s\x00%s\x00%s", s.URL, s.SSE, s.CredentialHeader, s.CredentialPrefix, s.File)
		if old, ok := p.appliedMCPSig[k]; !ok || old != sigs[k] {
			changed[k] = true
		}
	}
	p.appliedMCPFile = next
	p.appliedMCPSig = sigs
	p.probes = probes
	var fresh []Probe
	for _, pr := range probes {
		if changed[uuidKey(pr.Server)] {
			fresh = append(fresh, pr)
		}
	}
	live := map[gateway.AgentRef]bool{}
	for _, s := range servers {
		live[s.probeAgent()] = true
	}
	for ref := range p.probeKeys {
		if !live[ref] {
			delete(p.probeKeys, ref)
		}
	}
	return fresh
}

func (s *MCPServer) target() string { return mcpservers.TargetName(s.Org.Slug, s.Name) }

func (s *MCPServer) probeAgent() gateway.AgentRef {
	return gateway.AgentRef{Org: s.Org.Slug, Name: gateway.MCPProbeAgentPrefix + s.Name}
}

// uuidKey is the canonical string of an ID, the key of the applied-file maps.
func uuidKey(id pgtype.UUID) string { return execution.UUIDString(id.Bytes) }

// mcpTarget is the server's gateway target.
func (p *Planner) mcpTarget(s *MCPServer) gateway.MCPTarget {
	t := gateway.MCPTarget{Name: s.target(), URL: s.URL, SSE: s.SSE, Org: s.Org.Slug}
	if s.File != "" {
		t.CredentialHeader = s.CredentialHeader
		t.CredentialPrefix = s.CredentialPrefix
		t.CredentialFile = path.Join(p.cfg.KeyDir, s.File)
	}
	return t
}

// mcpGrants returns the agent's grants on the served servers, by tier. Servers that are unhealthy give
// nothing.
func mcpGrants(allowed []Allowed, servers []*MCPServer) (direct, ask []gateway.ToolGrant) {
	for _, a := range allowed {
		i := slices.IndexFunc(servers, func(s *MCPServer) bool { return s.ID == a.Server })
		if i < 0 {
			continue
		}
		s := servers[i]
		if s.Unhealthy {
			continue
		}
		var auto, asked []string
		for _, t := range s.Tools {
			if !t.Present || (a.Tools != nil && !slices.Contains(a.Tools, t.Name)) {
				continue
			}
			if mcpservers.ToolNameProblem(s.Org.Slug, s.Name, t.Name) != "" {
				continue // agents cannot use a name this long or with these characters
			}
			switch t.Tier {
			case TierAuto:
				auto = append(auto, t.Name)
			case TierAsk:
				asked = append(asked, t.Name)
			case TierNever:
			}
		}
		if len(auto) > 0 {
			direct = append(direct, gateway.ToolGrant{Target: s.target(), Tools: auto})
		}
		if len(asked) > 0 {
			asked = append(asked, mcpservers.AskStatusTool)
			ask = append(ask, gateway.ToolGrant{Target: mcpservers.AskTargetName(s.Org.Slug, s.Name), Tools: asked})
		}
	}
	return direct, ask
}

// applyMCP adds the served servers, the ask-tier targets, the agents' MCP grants and the prober's keys
// to the state. It returns the probes the prober should run once the state is applied.
func (pl *Plan) applyMCP(ctx context.Context, lk Lookups) ([]Probe, error) {
	p, state := pl.p, &pl.State
	if len(pl.servers) == 0 {
		return nil, nil
	}
	askTarget := map[string]bool{}
	for _, a := range pl.in.Agents {
		if !a.Active || a.Key.ID == "" {
			continue
		}
		direct, ask := mcpGrants(pl.allowances[a.Org.Slug][a.ID], pl.servers)
		if len(ask) > 0 && p.cfg.MCPAskURL == "" {
			p.log.Warn("an agent is allowed MCP tools in the ask tier but no approval proxy is configured; they are not offered", "org", a.Org.Slug, "agent", uuidKey(a.ID))
			ask = nil
		}
		for _, g := range ask {
			askTarget[g.Target] = true
		}
		grants := append(slices.Clone(state.Tools[a.Ref]), append(direct, ask...)...)
		if len(grants) > 0 {
			state.GrantTools(a.Ref, grants...)
		}
	}
	var probes []Probe
	execGrants := map[string][]gateway.ToolGrant{}
	execOrg := map[string]Org{}
	for _, s := range pl.servers {
		// The executor makes calls a person approved, and an approval can come from either tier: an
		// ask-tier call (ADR 0042) or an auto-tier call the leak guard sent to a person (ADR 0091
		// decision 5). So it opens every present tool of the auto and ask tiers whose name agents
		// can see, and never a tool that is in the never tier, absent or unnamable. The key is
		// koinon-api's alone and runs only approved calls with their exact arguments, so this
		// gives no agent a tool. What an approval may still run when it executes (the server, the
		// tool's content, the never tier, the agent's allowance) is checked in koinon-api
		// (approvals.executeMCP), not here.
		var approvable []string
		for _, t := range s.Tools {
			if t.Present && (t.Tier == TierAuto || t.Tier == TierAsk) && mcpservers.ToolNameProblem(s.Org.Slug, s.Name, t.Name) == "" {
				approvable = append(approvable, t.Name)
			}
		}
		execOrg[s.Org.Slug] = s.Org
		if len(approvable) > 0 {
			execGrants[s.Org.Slug] = append(execGrants[s.Org.Slug], gateway.ToolGrant{Target: s.target(), Tools: approvable})
		}
	}
	// Each org with servers gets an executor key, for the calls a human approved: they go through the
	// gateway like any call (its egress policy, its credential injection), so koinon-api never holds a
	// server credential. The key opens that org's servers only (the target conditions).
	for _, slug := range slices.Sorted(maps.Keys(execOrg)) {
		ref := gateway.AgentRef{Org: slug, Name: gateway.MCPExecAgentName}
		key, err := lk.SystemKey(ctx, mcpservers.ExecSecretName(execOrg[slug].ID), execOrg[slug], ref)
		if err != nil {
			return nil, err
		}
		state.AddKey(key)
		state.GrantTools(ref, execGrants[slug]...)
	}
	for _, s := range pl.servers {
		state.MCP = append(state.MCP, p.mcpTarget(s))
		if at := mcpservers.AskTargetName(s.Org.Slug, s.Name); askTarget[at] {
			state.MCP = append(state.MCP, gateway.MCPTarget{Name: at, URL: strings.TrimRight(p.cfg.MCPAskURL, "/") + "/" + s.Name, Org: s.Org.Slug})
		}
		// The prober's key: it lists the server's tools through the gateway, as an agent would.
		ref := s.probeAgent()
		key, material, err := p.probeKey(ctx, lk, ref)
		if err != nil {
			return nil, err
		}
		state.AddKey(key)
		state.GrantTools(ref, gateway.ToolGrant{Target: s.target(), All: true})
		probes = append(probes, Probe{
			Org: s.Org.ID, OrgSlug: s.Org.Slug, Server: s.ID, Name: s.Name, Target: s.target(),
			Material: material,
		})
	}
	return probes, nil
}

// probeKey returns the prober's key for ref, issuing it on first use. The material lives in this
// process only: a restart issues new keys and so revokes the old ones.
func (p *Planner) probeKey(ctx context.Context, lk Lookups, ref gateway.AgentRef) (gateway.Key, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.probeKeys == nil {
		p.probeKeys = map[gateway.AgentRef]gateway.IssuedKey{}
	}
	if k, ok := p.probeKeys[ref]; ok {
		return k.Key, k.Material, nil
	}
	k, err := lk.IssueKey(ctx, ref)
	if err != nil {
		return gateway.Key{}, "", fmt.Errorf("issue the MCP probe key of %s: %w", ref, err)
	}
	p.probeKeys[ref] = k
	return k.Key, k.Material, nil
}

// applyModelCheck gives each org that serves a priced model its model-check key (ADR 0056 amendment,
// ADR 0072): the system principal for the provider echo check. The key requests only the models the
// state serves to the org, and carries the net of its small daily budget and the org's remaining caps,
// so a runaway caller is stopped by the gateway (standalone) or by the key being taken out
// (Kubernetes mode) like an agent's key. An org that serves no priced model has no key. A budget that
// cannot be computed fails the key closed to zero for this pass. The errors of the orgs it could not
// serve are Problems; none stops the pass.
func (pl *Plan) applyModelCheck(ctx context.Context, lk Lookups) {
	state := &pl.State
	for _, org := range pl.in.Orgs {
		names := state.ServedModels(org.Slug)
		if len(names) == 0 {
			continue
		}
		ref := gateway.AgentRef{Org: org.Slug, Name: gateway.ModelCheckAgentName}
		key, err := lk.SystemKey(ctx, mcpservers.ModelCheckSecretName(org.ID), org, ref)
		if err != nil {
			pl.Problems = append(pl.Problems, Problem{ProblemProvider, fmt.Errorf("model-check key of org %s: %w", org.Slug, err)})
			continue
		}
		state.AddKey(key)
		state.RestrictModels(ref, names...)
		l, err := lk.SystemLimit(ctx, org, pl.in.Now)
		if err != nil {
			pl.Problems = append(pl.Problems, Problem{ProblemSpendLimit, fmt.Errorf("model-check budget of org %s not computed (key fails closed): %w", org.Slug, err)})
			l = failedLimit()
		}
		state.SetLimit(ref, l)
	}
}
