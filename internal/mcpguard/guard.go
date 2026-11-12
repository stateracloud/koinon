// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// DefaultByteCap is the byte cap per run of ADR 0091 decision 5: payload bytes a run that holds
// both legs may send through external-low sinks. "About 2 KB" until real traffic sets it.
const DefaultByteCap = 2048

// LookupBudget is the time the guard allows itself besides the taint check's own backstop: the
// identity, declaration, session and run reads and writes.
const LookupBudget = 2 * time.Second

// Deadline bounds one decision: the taint check's 5 s backstop plus LookupBudget. It must stay
// under GatewayProcessorTimeout, or the gateway gives up first and the agent reads a bare
// -32603 instead of the guard's reason.
const Deadline = 5*time.Second + LookupBudget

// DefaultFeedWait bounds how long the guard waits for agentd's feed to record the tool results the
// harness has finished (ADR 0091 decision 4, amendment 2026-11-03). Past it the session is read as
// holding both legs: the call gets a card or a refusal, never a pass on legs that may be stale.
const DefaultFeedWait = 2 * time.Second

// feedPoll is how often the guard rereads the feed's counts while it waits.
const feedPoll = 20 * time.Millisecond

// GatewayProcessorTimeout is agentgateway's fixed timeout of a remote guardrail processor (v1.7.0,
// crates/agentgateway/src/mcp/guardrails/client.rs: with_default_timeout, 10 s). It has no
// configuration field; third_party/agentgateway/README.md records where to check it.
const GatewayProcessorTimeout = 10 * time.Second

// Options configures a Guard.
type Options struct {
	Logger  *slog.Logger
	Metrics *Metrics
	Now     func() time.Time
	// ByteCap is the byte cap per run (DefaultByteCap when <= 0).
	ByteCap int
	// Limits bound the taint check (leakguard defaults for zero fields).
	Limits leakguard.Limits
	// MaxFingerprints bounds one session's taint store (DefaultMaxFingerprints when <= 0).
	MaxFingerprints int
	// Decider decides the calls the rules leave open (NeedsDecision). Nil is a PersonDecider with
	// Approve: an ADR 0042 approval of the exact call.
	Decider Decider
	// Approve records approvals for the default PersonDecider. Without it, and without a Decider,
	// a call that needs a decision is refused.
	Approve ApproveFunc
	// FeedWait bounds the wait for agentd's feed (DefaultFeedWait when <= 0).
	FeedWait time.Duration
	// OwnTargets are the gateway targets of Koinon's own tool servers (the code-host tools and
	// gateway.KoinonTarget). The guard lets the agent's calls to them through: their results carry
	// no org data, and a call to one only records the write, which runs when its stored decision
	// is a person's ADR 0042 approval of the exact call. A code-host write that no person decides
	// (the Trusted preset allows it, an approval scope covers it) is not exempt: githubtools runs
	// it only when CheckCodeHost finds it clean (githubtools.Options.LeakCheck), and records it
	// for a person's approval otherwise (#687). Any other target that is not an org's server is
	// refused.
	OwnTargets []string
	// UnseenAfter is how long a passed call may wait for its guarded result before both legs are
	// recorded on its session (DefaultUnseenAfter when <= 0; #254).
	UnseenAfter time.Duration
}

// Guard is the MCP leak guard of ADR 0084 and ADR 0091, behind agentgateway's mcpGuardrails.
type Guard struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	state   *State
	log     *slog.Logger
	metrics *Metrics
	now     func() time.Time
	cap     int
	limits  leakguard.Limits
	decider Decider
	person  Decider
	own     []string
	url     leakguard.URLPolicy
	// feedWait bounds the wait for agentd's feed; feedPolled, when set, runs before each wait step
	// (a test seam).
	feedWait   time.Duration
	feedPolled func()
	// unseenAfter: see Options.UnseenAfter.
	unseenAfter time.Duration
}

// New returns a Guard on pool.
func New(pool *pgxpool.Pool, o Options) *Guard {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ByteCap <= 0 {
		o.ByteCap = DefaultByteCap
	}
	if o.Decider == nil {
		o.Decider = PersonDecider{Approve: o.Approve}
	}
	if o.FeedWait <= 0 {
		o.FeedWait = DefaultFeedWait
	}
	if o.UnseenAfter <= 0 {
		o.UnseenAfter = DefaultUnseenAfter
	}
	return &Guard{
		pool: pool, q: store.New(pool), state: NewState(pool, o.MaxFingerprints, o.Metrics), log: o.Logger, metrics: o.Metrics,
		now: o.Now, cap: o.ByteCap, limits: o.Limits, decider: o.Decider, person: PersonDecider{Approve: o.Approve}, own: slices.Clone(o.OwnTargets),
		url: leakguard.DefaultURLPolicy(), feedWait: o.FeedWait, unseenAfter: o.UnseenAfter,
	}
}

// codeHostWrite is the declaration of a write of Koinon's own code-host tools: its class follows
// from the repository (ADR 0091 decision 3), every argument of the call is payload.
var codeHostWrite = sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}

// CheckCodeHost is the leak check of a call to Koinon's own code-host tools that no person decides
// (githubtools.Options.LeakCheck): the Trusted preset allows it, or an approval scope covers it. The
// call is checked as a code-host write of an org's MCP server: the repository is its address,
// looked up in the list of the connection the grant is bound to, and the arguments are its payload.
// It writes nothing. A nil risk is a clean call. Otherwise the risk is for the card of a person's
// approval of the exact call: a call the rules hold (it needs a decision or would be refused) and
// a call whose facts the guard cannot gather both go to a person, as the same call does under a
// preset a person decides. An error is a call the guard could not check at all.
func (g *Guard) CheckCodeHost(ctx context.Context, call githubtools.LeakCall) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	raw, err := codeHostArgs(call)
	if err != nil {
		return nil, err
	}
	c := caller{org: call.Org, agent: call.Agent, server: store.McpServer{OrgID: call.Org.ID, CodeHostConnectionID: call.Connection}}
	a, err := g.assess(ctx, c, codeHostWrite, raw, false)
	if err != nil {
		return nil, err
	}
	if a.refused != nil {
		a.verdict = a.refused.Verdict
	}
	v := a.verdict
	if v.Action == Pass {
		return nil, nil
	}
	if len(v.Reasons) == 0 {
		a.verdict.Reasons = []string{v.Reason}
	}
	g.log.Info("mcp guard: an own code-host call goes to a person", "org", call.Org.Slug, "agent", call.Agent.ID.String(),
		"tool", call.Tool, "action", string(v.Action), "reason", v.Reason, "reasons", a.verdict.Reasons, "class", string(a.class),
		"feed_stalled", a.feedStalled, "reply_unseen", a.replyUnseen)
	return a.risk(), nil
}

// codeHostArgs are a code-host call's arguments with its repository as the owner and repo address
// arguments of codeHostWrite. Arguments that already name either are not read: the address would
// be ambiguous.
func codeHostArgs(call githubtools.LeakCall) (json.RawMessage, error) {
	owner, repo, ok := strings.Cut(call.Repo, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("mcpguard: the repository %q is not owner/name", call.Repo)
	}
	args := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(call.Arguments)) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("mcpguard: the arguments of %s: %w", call.Tool, err)
		}
	}
	for k := range args {
		for _, a := range codeHostWrite.AddressArgs {
			if strings.EqualFold(k, a) {
				return nil, fmt.Errorf("mcpguard: the arguments of %s name %q, the repository's address argument", call.Tool, k)
			}
		}
	}
	var err error
	if args["owner"], err = json.Marshal(owner); err != nil {
		return nil, err
	}
	if args["repo"], err = json.Marshal(repo); err != nil {
		return nil, err
	}
	return json.Marshal(args)
}

// State is the guard's Postgres state, for the taint feed.
func (g *Guard) State() *State { return g.state }

// Call is one tools/call as the gateway reports it.
type Call struct {
	// Target is the gateway target (McpRequest.service_names): "<org>-<server>", its "-ask" twin, or
	// one of Koinon's own targets.
	Target string
	// Agent is the authenticated key's agent, "<org slug>/<agent>" (CEL apiKey.agent).
	Agent string
	// Tool is the tool's own name, without the target prefix.
	Tool      string
	Arguments json.RawMessage
}

// Outcome is the guard's answer to one call.
type Outcome struct {
	Verdict Verdict
	// Text is what the agent reads when the call does not pass: the refusal's reason or the
	// approval's status line.
	Text string
}

func refuse(reason, text string) Outcome {
	return Outcome{Verdict: Verdict{Action: Refuse, Reason: reason, Detail: text}, Text: text}
}

// errText is the refusal of a call the guard could not decide (fail closed).
const errText = "refused by the leak guard: it could not check this call (an internal error); try again later"

// caller is the identity and target of one call.
type caller struct {
	ref    gateway.AgentRef
	org    store.Org
	agent  store.Agent
	server store.McpServer
	ask    bool // the "-ask" target: mcpask records the card
	own    bool // one of Koinon's own targets
	exempt bool // Koinon's executor or probe
}

// agentOf resolves the org and agent a call's identity names. A non-nil outcome is a refusal.
func (g *Guard) agentOf(ctx context.Context, ref gateway.AgentRef) (store.Org, store.Agent, *Outcome, error) {
	org, err := g.q.GetOrgBySlug(ctx, ref.Org)
	if errors.Is(err, pgx.ErrNoRows) {
		o := refuse(ReasonIdentity, "refused by the leak guard: unknown org")
		return store.Org{}, store.Agent{}, &o, nil
	}
	if err != nil {
		return store.Org{}, store.Agent{}, nil, err
	}
	var id pgtype.UUID
	if err := id.Scan(ref.Name); err != nil {
		o := refuse(ReasonIdentity, "refused by the leak guard: the call does not come from an agent of Koinon")
		return store.Org{}, store.Agent{}, &o, nil
	}
	a, err := g.q.GetAgent(ctx, store.GetAgentParams{OrgID: org.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		o := refuse(ReasonIdentity, "refused by the leak guard: unknown agent")
		return store.Org{}, store.Agent{}, &o, nil
	}
	if err != nil {
		return store.Org{}, store.Agent{}, nil, err
	}
	return org, a, nil, nil
}

func (g *Guard) identify(ctx context.Context, agent, target string) (caller, *Outcome, error) {
	ref, err := gateway.ParseAgentRef(agent)
	if err != nil {
		o := refuse(ReasonIdentity, "refused by the leak guard: the call does not carry an agent identity")
		return caller{}, &o, nil
	}
	c := caller{ref: ref}
	if gateway.IsInternalAgent(ref) {
		// Koinon's executor runs calls a person approved; the prober lists tools. Neither is an
		// agent's session.
		c.exempt = true
		return c, nil, nil
	}
	if slices.Contains(g.own, target) {
		c.own = true
		return c, nil, nil
	}
	org, a, o, err := g.agentOf(ctx, ref)
	if o != nil || err != nil {
		return caller{}, o, err
	}
	name, ok := strings.CutPrefix(target, org.Slug+"-")
	if !ok {
		o := refuse(ReasonIdentity, "refused by the leak guard: the tool server is not one of this org's")
		return caller{}, &o, nil
	}
	if base, isAsk := strings.CutSuffix(name, mcpservers.AskSuffix); isAsk {
		name, c.ask = base, true
	}
	srv, err := g.q.GetServedMCPServerByName(ctx, store.GetServedMCPServerByNameParams{OrgID: org.ID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		o := refuse(ReasonIdentity, fmt.Sprintf("refused by the leak guard: the org has no MCP server %s", name))
		return caller{}, &o, nil
	}
	if err != nil {
		return caller{}, nil, err
	}
	c.org, c.agent, c.server = org, a, srv
	return c, nil, nil
}

// Check decides one tools/call (ADR 0091 decision 5). An error inside the guard is a refusal: the
// guard fails closed, as the gateway does when the guard cannot be reached.
func (g *Guard) Check(ctx context.Context, call Call) Outcome {
	start := g.now()
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	var f decisionFacts
	out, err := g.check(ctx, call, &f)
	if err == nil && out.Verdict.Action == Pass && f.opens != nil {
		// The call goes to its server: its reply must reach the guard, or the session takes both
		// legs (#254). A call the guard cannot open is refused, as any check that fails.
		err = g.openCall(ctx, *f.opens, call.Target, call.Tool)
	}
	if err != nil {
		g.log.Error("mcp guard: check failed", "target", call.Target, "tool", call.Tool, "err", err)
		out = refuse(ReasonError, errText)
	}
	class := string(f.class)
	g.metrics.decided(out.Verdict, class, g.now().Sub(start))
	g.logDecision(call, out, f)
	return out
}

// decisionFacts is what the guard knew when it decided a call, for its log line: never the
// arguments or the payload.
type decisionFacts struct {
	class sinkclass.Class
	// legs is set once the session's legs were read.
	legs *Legs
	// run is "found" or "none" once the guard looked for the agent's running run, else empty.
	run         string
	feedStalled bool
	replyUnseen bool
	// opens is the caller whose session opens the call once it passes: set when its result would
	// set a leg (#254).
	opens *caller
}

// logDecision logs every refusal and every call sent to a person (an ask target or an approval),
// at info: the reason codes, the class, the legs, whether a run was found and whether the feed
// stalled. No argument value and no payload is logged.
func (g *Guard) logDecision(call Call, out Outcome, f decisionFacts) {
	v := out.Verdict
	if v.Action == Pass && v.Reason != ReasonAskTarget {
		return
	}
	attrs := []any{
		"target", call.Target, "tool", call.Tool, "action", string(v.Action), "reason", v.Reason,
		"reasons", v.Reasons, "class", string(f.class), "run", f.run, "feed_stalled", f.feedStalled,
		"reply_unseen", f.replyUnseen,
	}
	if f.legs != nil {
		attrs = append(attrs, "untrusted_input", f.legs.UntrustedInput, "private_data", f.legs.PrivateData)
	}
	g.log.Info("mcp guard: decided", attrs...)
}

func (g *Guard) check(ctx context.Context, call Call, f *decisionFacts) (Outcome, error) {
	c, refused, err := g.identify(ctx, call.Agent, call.Target)
	if err != nil {
		return Outcome{}, err
	}
	if refused != nil {
		return *refused, nil
	}
	if c.exempt {
		return Outcome{Verdict: Verdict{Action: Pass, Reason: ReasonExempt}}, nil
	}
	if c.own {
		// An own tool nobody declared brings both legs (OwnResultLegs), so its reply must be seen too.
		if OwnResultLegs(call.Tool).Any() {
			org, agent, o, err := g.agentOf(ctx, c.ref)
			if err != nil {
				return Outcome{}, err
			}
			if o == nil {
				c.org, c.agent = org, agent
				f.opens = &c
			}
		}
		return Outcome{Verdict: Verdict{Action: Pass, Reason: ReasonExempt}}, nil
	}
	tool, decl, err := g.tool(ctx, c, call.Tool)
	if err != nil {
		return Outcome{}, err
	}
	if resultLegs(c.server, decl).Any() {
		f.opens = &c
	}
	a, err := g.assess(ctx, c, decl, call.Arguments, true)
	f.class, f.run, f.feedStalled, f.replyUnseen = a.class, a.run, a.feedStalled, a.replyUnseen
	if a.legsRead {
		l := a.legs
		f.legs = &l
	}
	if err != nil {
		return Outcome{}, err
	}
	if a.refused != nil {
		return *a.refused, nil
	}
	v := a.verdict
	switch v.Action {
	case Pass:
		return Outcome{Verdict: v}, nil
	case Refuse:
		return Outcome{Verdict: v, Text: v.Detail}, nil
	}
	// NeedsDecision
	if c.ask {
		// The call's tool is ask-tier: mcpask records the one approval it gets, with the guard's
		// reasons on its card (mcpask asks Assess); two cards for one call would be noise (ADR 0091
		// decision 5). A person decides it either way.
		return Outcome{Verdict: Verdict{Action: Pass, Reason: ReasonAskTarget, Reasons: v.Reasons}}, nil
	}
	if !tool.Valid {
		return refuse(v.Reason, "refused by the leak guard: this call needs a decision, and the server never published this tool"), nil
	}
	// An approval belongs to a run (ADR 0042): a call that needs one outside a task is refused, in
	// words, rather than failed as an error of the approval store.
	if _, err := g.q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: c.org.ID, AgentID: c.agent.ID}); errors.Is(err, pgx.ErrNoRows) {
		f.run = "none"
		o := refuse(ReasonNoRun, NoRunApprovalText)
		o.Verdict.Reasons = append(slices.Clone(v.Reasons), ReasonNoRun)
		return o, nil
	} else if err != nil {
		return Outcome{}, err
	}
	f.run = "found"
	// Only a call whose one reason is both legs may reach the Decider seam (ADR 0092: the judge
	// sees facts, not text). A text match carries private text out, and a stalled feed means the
	// legs were not read: a person decides those, always.
	decider := g.decider
	if !onlyBothLegs(v) {
		decider = g.person
	}
	out, err := decider.DecidePending(ctx, Pending{
		Org: c.org, Agent: c.agent, Server: c.server, Tool: tool.McpTool, Arguments: call.Arguments,
		Verdict: v, Destination: a.args.Address, PayloadBytes: a.args.PayloadBytes(), Risk: a.risk(),
	})
	if err != nil {
		return Outcome{}, err
	}
	if out.Verdict.Action == NeedsDecision || out.Verdict.Action == "" {
		// A Decider must decide; anything else fails closed.
		return Outcome{}, fmt.Errorf("mcpguard: the decider left the call undecided (%q)", out.Verdict.Action)
	}
	return out, nil
}

// knownTool is a tool row, or none for a name the server never published.
type knownTool struct {
	store.McpTool
	Valid bool
}

// tool returns the tool's row and declaration; an unknown tool has the default declaration.
func (g *Guard) tool(ctx context.Context, c caller, name string) (knownTool, sinkclass.Declaration, error) {
	t, err := g.q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: c.org.ID, ServerID: c.server.ID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return knownTool{}, sinkclass.Default(), nil
	}
	if err != nil {
		return knownTool{}, sinkclass.Declaration{}, err
	}
	return knownTool{McpTool: t, Valid: true}, declarationOf(t), nil
}

func declarationOf(t store.McpTool) sinkclass.Declaration {
	d := sinkclass.Declaration{
		Class: sinkclass.Class(t.SinkClass), Derivation: sinkclass.Derivation(t.SinkDerivation),
		AddressArgs: slices.Clone(t.AddressArgs),
	}
	if t.ResultUntrustedInput.Valid {
		v := t.ResultUntrustedInput.Bool
		d.ResultUntrustedInput = &v
	}
	if t.ResultPrivateData.Valid {
		v := t.ResultPrivateData.Bool
		d.ResultPrivateData = &v
	}
	return d
}

// assessment is the facts and verdict of one call.
type assessment struct {
	class   sinkclass.Class
	args    Args
	legs    Legs
	verdict Verdict
	refused *Outcome
	// legsRead: legs holds the session's legs (the call was external).
	legsRead bool
	// run is "found" or "none" once the agent's running run was looked up.
	run string
	// feedStalled: agentd's feed did not catch up in time, so the legs were taken as both.
	feedStalled bool
	// replyUnseen: a call the guard passed has no result it saw yet, so the legs were taken as both
	// (#254).
	replyUnseen bool
}

func (a assessment) risk() map[string]any {
	leak := map[string]any{
		"reasons": a.verdict.Reasons, "sink_class": string(a.class), "destination": a.args.Address,
		"payload_bytes": a.args.PayloadBytes(),
		"legs":          map[string]bool{"untrusted_input": a.legs.UntrustedInput, "private_data": a.legs.PrivateData},
	}
	if a.feedStalled {
		leak["feed_stalled"] = true
	}
	if a.replyUnseen {
		leak["reply_unseen"] = true
	}
	if m := a.verdict.Match; m != nil {
		leak["match"] = map[string]any{"excerpt": m.Excerpt, "encoding": m.Encoding, "length": m.Length}
	}
	return map[string]any{"leak": leak}
}

// assess gathers the facts of one call and decides it. With charge, the run's budgets are charged
// (the gateway's check); without, nothing is written (mcpask's card).
func (g *Guard) assess(ctx context.Context, c caller, decl sinkclass.Declaration, raw json.RawMessage, charge bool) (assessment, error) {
	var a assessment
	pol, err := loadPolicy(ctx, g.q, c.org)
	if err != nil {
		return a, err
	}
	args, err := SplitArgs(raw, decl, pol.hosts())
	if err != nil {
		reason := ReasonCheck
		if errors.Is(err, ErrAmbiguousAddress) {
			reason = ReasonAmbiguous
		}
		o := refuse(reason, "refused by the leak guard: "+err.Error())
		a.refused = &o
		return a, nil
	}
	a.args = args
	a.class = pol.classify(c.server, decl, args.Address, g.now())
	if a.class == sinkclass.None || a.class == sinkclass.Internal {
		a.verdict = Decide(Facts{Class: a.class})
		return a, nil
	}
	gen, ok, err := Generation(ctx, g.q, c.org.ID, c.agent.ID)
	if err != nil {
		return a, err
	}
	if !ok {
		o := refuse(ReasonNoRun, "refused by the leak guard: this agent has no sandbox session yet, so its calls out of the org cannot be checked")
		a.refused = &o
		return a, nil
	}
	// The legs must include every tool result the harness finished before this call: agentd may not
	// have recorded the last one yet (its updates and the call travel different ways).
	stalled, live, err := g.awaitFeed(ctx, c)
	if err != nil {
		return a, err
	}
	a.run = "none"
	if live {
		a.run = "found"
	}
	sess := Session{Org: c.org.ID, Agent: c.agent.ID, Generation: gen}
	row, err := g.state.Ensure(ctx, sess)
	if err != nil {
		return a, err
	}
	a.legs, a.legsRead = Legs{UntrustedInput: row.UntrustedInput, PrivateData: row.PrivateData}, true
	if stalled {
		g.log.Warn("mcp guard: agentd's feed did not catch up; the call is decided as if the session held both legs",
			"org", c.org.Slug, "agent", c.agent.ID.String(), "wait", g.feedWait)
		a.legs, a.feedStalled = Legs{UntrustedInput: true, PrivateData: true}, true
	}
	// A reply the guard never saw counts as both legs (#254): for good once its call is older than
	// unseenAfter (charged checks only; mcpask's card writes nothing), and for this decision while any
	// call is still open.
	persisted, open, err := g.unseenReplies(ctx, sess, charge)
	if err != nil {
		return a, err
	}
	if persisted != nil && !a.feedStalled {
		a.legs = *persisted
	}
	if open > 0 {
		a.legs, a.replyUnseen = Legs{UntrustedInput: true, PrivateData: true}, true
	}
	fp, err := Fingerprinter(row, pol.threshold())
	if err != nil {
		return a, err
	}
	// The text match: payload only, each string alone and joined in canonical order (ADR 0091
	// decision 6, #256).
	taint, err := leakguard.TaintChecker{Store: &taintStore{q: g.q, sess: sess, evicted: row.Evicted}, Limits: g.limits}.CheckJoins(ctx, sess.scope(), fp, args.Payload, args.Canonical, args.Values, args.CallValues)
	if err != nil {
		return a, err
	}
	if taint.Evicted {
		g.log.Warn("mcp guard: the session's taint store dropped fingerprints; a miss may be text it once held",
			"org", c.org.Slug, "agent", c.agent.ID.String(), "generation", gen)
	}
	f := Facts{Class: a.class, BothLegs: a.legs.Both(), Taint: taint, Cap: g.cap, PayloadBytes: args.PayloadBytes()}
	if a.class != sinkclass.ExternalLow || !charge {
		a.verdict = a.stallMarked(Decide(f))
		return a, nil
	}
	// external-low: the URL rule and the byte cap, charged to the run under its lock.
	run, err := g.q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: c.org.ID, AgentID: c.agent.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		o := refuse(ReasonNoRun, "refused by the leak guard: calls out of the org are only allowed while the agent works on a task")
		a.refused = &o
		return a, nil
	}
	if err != nil {
		return a, err
	}
	err = pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		budget, err := q.LockLeakGuardRun(ctx, store.LockLeakGuardRunParams{OrgID: c.org.ID, RunID: run.ID})
		if err != nil {
			return err
		}
		st := leakguard.NewRunState()
		if len(budget.UrlState) > 0 {
			if err := json.Unmarshal(budget.UrlState, st); err != nil {
				return err
			}
		}
		var capped int
		a.verdict, capped = g.decideExternalLow(st, int(budget.CappedBytes), c.server, decl, args, f)
		a.verdict = a.stallMarked(a.verdict)
		if a.verdict.Action == Refuse {
			return nil // nothing is charged for a call that does not leave
		}
		state, err := json.Marshal(st)
		if err != nil {
			return err
		}
		return q.SaveLeakGuardRun(ctx, store.SaveLeakGuardRunParams{OrgID: c.org.ID, RunID: run.ID, CappedBytes: int32(min(capped, 1<<30)), UrlState: state}) //nolint:gosec // bounded
	})
	return a, err
}

// decideExternalLow decides an external-low call (ADR 0091 decision 5): the ADR 0084 URL rule,
// which charges st, and, once the session holds both legs, the byte cap per run, where used is what
// the run has sent since it held them. It returns the verdict and the run's count after the call. A
// call that is refused leaves the run as it was: the caller saves neither st nor the count. The
// replay suite drives it without Postgres, so the cap it checks is this one.
func (g *Guard) decideExternalLow(st *leakguard.RunState, used int, srv store.McpServer, decl sinkclass.Declaration, args Args, f Facts) (Verdict, int) {
	f.URL, f.URLChecked = g.urlRule(st, srv, decl, args)
	f.CapUsed = used
	if f.BothLegs {
		f.OverCap = used+f.PayloadBytes > g.cap
		used += f.PayloadBytes
	}
	return Decide(f), used
}

// stallMarked adds ReasonFeedStalled to a verdict that is open while the feed stalled, and
// ReasonReplyUnseen to one open while a reply was unseen: the legs were assumed, not read, so the
// call goes to a person and never to a Decider (onlyBothLegs).
func (a assessment) stallMarked(v Verdict) Verdict {
	if v.Action != NeedsDecision {
		return v
	}
	if a.feedStalled {
		v.Reasons = append(slices.Clone(v.Reasons), ReasonFeedStalled)
	}
	if a.replyUnseen {
		v.Reasons = append(slices.Clone(v.Reasons), ReasonReplyUnseen)
	}
	return v
}

// awaitFeed waits until agentd has recorded every finished tool call of the agent's live run it
// had read from the harness when the call arrived (leak_guard_runs.feed_done reaching that
// feed_read). The live run includes its end-of-turn summary: the harness still runs then. It
// reports stalled when the feed did not catch up within feedWait (the caller then takes the session
// as holding both legs), and live when the agent has a running run. Without a running run, or a feed
// for it, there is nothing to wait for: no harness of the agent is running, and the legs it left
// are recorded on the generation.
func (g *Guard) awaitFeed(ctx context.Context, c caller) (stalled, live bool, err error) {
	run, err := g.q.GetLiveRunForAgent(ctx, store.GetLiveRunForAgentParams{OrgID: c.org.ID, AgentID: c.agent.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	key := store.GetLeakGuardFeedParams{OrgID: c.org.ID, RunID: run.ID}
	feed, err := g.q.GetLeakGuardFeed(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, true, nil
	}
	if err != nil {
		return false, true, err
	}
	target := feed.FeedRead
	if feed.FeedDone >= target {
		return false, true, nil
	}
	deadline := time.NewTimer(g.feedWait)
	defer deadline.Stop()
	tick := time.NewTicker(feedPoll)
	defer tick.Stop()
	for {
		if g.feedPolled != nil {
			g.feedPolled()
		}
		select {
		case <-ctx.Done():
			return false, true, ctx.Err()
		case <-deadline.C:
			return true, true, nil
		case <-tick.C:
		}
		feed, err = g.q.GetLeakGuardFeed(ctx, key)
		if err != nil {
			return false, true, err
		}
		if feed.FeedDone >= target {
			return false, true, nil
		}
	}
}

// urlRule applies ADR 0084 decision 5 to an external-low call: a fetch tool's URL, a search
// server's query, and any other address argument that is an http or https URL. It charges st.
func (g *Guard) urlRule(st *leakguard.RunState, srv store.McpServer, decl sinkclass.Declaration, args Args) (leakguard.Decision, bool) {
	checked := false
	for _, name := range decl.AddressArgs {
		v, ok := args.Address[name]
		if !ok {
			continue
		}
		if _, isURL := sinkclass.FetchHost(v); !isURL && decl.Derivation != sinkclass.Fetch {
			continue
		}
		checked = true
		if d := g.url.CheckURL(st, v); !d.Allow {
			return d, true
		}
	}
	if srv.Kind == store.McpServerKindSearch {
		checked = true
		if d := g.url.CheckSearch(st, strings.Join(args.Payload, " ")); !d.Allow {
			return d, true
		}
	}
	return leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}, checked
}

// onlyBothLegs reports whether v is open for the one reason the Decider seam may take: both legs,
// with no text match.
func onlyBothLegs(v Verdict) bool {
	return v.Reason == ReasonBothLegs && v.Match == nil && len(v.Reasons) == 1 && v.Reasons[0] == ReasonBothLegs
}
