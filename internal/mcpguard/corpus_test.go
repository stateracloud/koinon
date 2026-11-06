// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The replay corpus (#352, ADR 0091): one JSON object per line, in the order things happened.
//
//   - org (first): the org's leak-guard settings: threshold (0: the default), trusted_hosts (the
//     fetch-trusted list) and code_host_repos (the code-host scope: connection, owner, name,
//     visibility).
//   - server: an MCP server record: server (its name), kind (fetch, search or other), the legs its
//     results carry (untrusted_input, private_data) and code_host_connection (the connection ID it
//     is linked to, or none).
//   - tool: a tool's declaration (server, tool, declaration as the admin API stores it). A tool
//     without one has the default declaration (external-high, the server's legs).
//   - feed: what agentd reports of the sandbox's own tool results (a file read, shell output):
//     generation, run, tool, the legs it sets and the texts it fingerprints.
//   - call: one tools/call through the gateway: generation, run, server, tool, args (the arguments
//     object as sent) and result (the CallToolResult, absent when the call failed or was not made).
//
// A generation is one home generation of one agent (the session of the legs and the taint store); a
// run belongs to one generation. Hand-built corpora may add case and expect to a call; a recorded
// corpus never has them. The replay drives the guard's own decision path without Postgres: SplitArgs,
// the classification of classify.go, the legs of resultLegs, the text match on the payload, Decide,
// and for external-low decideExternalLow (the URL rule and the byte cap).

type corpusLine struct {
	Type string `json:"type"`

	Threshold     int          `json:"threshold,omitempty"`
	TrustedHosts  []string     `json:"trusted_hosts,omitempty"`
	CodeHostRepos []corpusRepo `json:"code_host_repos,omitempty"`

	Server             string `json:"server,omitempty"`
	Kind               string `json:"kind,omitempty"`
	UntrustedInput     bool   `json:"untrusted_input,omitempty"`
	PrivateData        bool   `json:"private_data,omitempty"`
	CodeHostConnection string `json:"code_host_connection,omitempty"`

	Tool        string                 `json:"tool,omitempty"`
	Declaration *sinkclass.Declaration `json:"declaration,omitempty"`

	Generation string          `json:"generation,omitempty"`
	Run        string          `json:"run,omitempty"`
	Seq        int             `json:"seq,omitempty"`
	Legs       corpusLegs      `json:"legs"`
	Texts      []string        `json:"texts,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`

	Case   string `json:"case,omitempty"`
	Expect string `json:"expect,omitempty"`
}

type corpusRepo struct {
	Connection string `json:"connection"`
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

type corpusLegs struct {
	UntrustedInput bool `json:"untrusted_input"`
	PrivateData    bool `json:"private_data"`
}

// readCorpus reads a corpus file.
func readCorpus(path string) ([]corpusLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []corpusLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		var l corpusLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// replayOptions are what a replay varies.
type replayOptions struct {
	// ByteCap is the cap per run (DefaultByteCap when 0; math.MaxInt32 to measure what is sent).
	ByteCap int
	// Threshold overrides the org's threshold when > 0.
	Threshold int
}

// replay is one org's guard in memory.
type replay struct {
	g         *Guard
	pol       orgPolicy
	servers   map[string]store.McpServer
	decls     map[string]sinkclass.Declaration
	sessions  map[string]*replaySession
	runs      map[string]*replayRun
	threshold int
	now       time.Time
}

type replaySession struct {
	legs  Legs
	store *leakguard.MemoryStore
	fp    *leakguard.Fingerprinter
	scope leakguard.Scope
}

type replayRun struct {
	st   *leakguard.RunState
	used int
}

// replayed is the guard's decision on one call, with what it read.
type replayed struct {
	line    corpusLine
	class   sinkclass.Class
	legs    Legs // before the call
	bytes   int  // payload bytes as the byte cap counts them
	verdict Verdict
}

// replayNow is the replay's clock: the code-host scope is read as fresh at it.
var replayNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// newReplay builds the org's guard from the corpus's org, server and tool lines.
func newReplay(lines []corpusLine, o replayOptions) (*replay, error) {
	r := &replay{
		g:       New(nil, Options{ByteCap: o.ByteCap}),
		servers: map[string]store.McpServer{}, decls: map[string]sinkclass.Declaration{},
		sessions: map[string]*replaySession{}, runs: map[string]*replayRun{}, now: replayNow,
	}
	for _, l := range lines {
		switch l.Type {
		case "org":
			lg := orgsettings.LeakGuard{MatchThreshold: l.Threshold}
			for _, h := range l.TrustedHosts {
				t, err := orgsettings.NormalizeTrustedEntry(h)
				if err != nil {
					return nil, err
				}
				lg.TrustedHosts = append(lg.TrustedHosts, t)
			}
			slices.Sort(lg.TrustedHosts)
			for _, c := range l.CodeHostRepos {
				lg.CodeHostRepos = append(lg.CodeHostRepos, sinkclass.Repo{
					Connection: c.Connection, Owner: strings.ToLower(c.Owner), Name: strings.ToLower(c.Name),
					Visibility: sinkclass.Visibility(c.Visibility), CheckedAt: replayNow,
				})
			}
			r.pol = orgPolicy{settings: lg}
		case "server":
			srv := store.McpServer{
				Name: l.Server, Kind: store.McpServerKind(l.Kind),
				ExposesUntrustedInput: l.UntrustedInput, ExposesPrivateData: l.PrivateData,
			}
			if l.CodeHostConnection != "" {
				if err := srv.CodeHostConnectionID.Scan(l.CodeHostConnection); err != nil {
					return nil, fmt.Errorf("server %s: %w", l.Server, err)
				}
			}
			r.servers[l.Server] = srv
		case "tool":
			if l.Declaration == nil {
				return nil, fmt.Errorf("tool %s/%s has no declaration", l.Server, l.Tool)
			}
			d, err := l.Declaration.Normalize()
			if err != nil {
				return nil, fmt.Errorf("tool %s/%s: %w", l.Server, l.Tool, err)
			}
			r.decls[l.Server+"/"+l.Tool] = d
		}
	}
	r.threshold = r.pol.threshold()
	if o.Threshold > 0 {
		r.threshold = o.Threshold
	}
	return r, nil
}

func (r *replay) session(gen string) (*replaySession, error) {
	if s, ok := r.sessions[gen]; ok {
		return s, nil
	}
	// A fixed key per generation: the replay must be the same on every machine. The real key is
	// random per session (state.go).
	key := sha256.Sum256([]byte("koinon-replay-key-not-a-secret/" + gen))
	fp, err := Fingerprinter(store.LeakGuardSession{FpKey: key[:]}, r.threshold)
	if err != nil {
		return nil, err
	}
	sess := Session{Org: pgtype.UUID{Valid: true}, Agent: pgtype.UUID{Valid: true}, Generation: gen}
	s := &replaySession{store: leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{}), fp: fp, scope: sess.scope()}
	r.sessions[gen] = s
	return s, nil
}

func (r *replay) run(id string) *replayRun {
	if x, ok := r.runs[id]; ok {
		return x
	}
	x := &replayRun{st: leakguard.NewRunState()}
	r.runs[id] = x
	return x
}

// record adds what a result or agentd's feed brings into the session: legs only grow, and private
// text is fingerprinted.
func (r *replay) record(ctx context.Context, s *replaySession, legs Legs, texts []string) error {
	s.legs.UntrustedInput = s.legs.UntrustedInput || legs.UntrustedInput
	s.legs.PrivateData = s.legs.PrivateData || legs.PrivateData
	if !legs.PrivateData {
		return nil
	}
	for _, t := range texts {
		if err := s.store.Add(ctx, s.scope, s.fp.Winnow(t)); err != nil {
			return err
		}
	}
	return nil
}

// apply replays one line. It returns the decision for a call line, nil for any other.
func (r *replay) apply(ctx context.Context, l corpusLine) (*replayed, error) {
	switch l.Type {
	case "feed":
		s, err := r.session(l.Generation)
		if err != nil {
			return nil, err
		}
		return nil, r.record(ctx, s, Legs{UntrustedInput: l.Legs.UntrustedInput, PrivateData: l.Legs.PrivateData}, l.Texts)
	case "call":
	default:
		return nil, nil
	}
	srv, ok := r.servers[l.Server]
	if !ok {
		return nil, fmt.Errorf("%s #%d: no server line for %q", l.Run, l.Seq, l.Server)
	}
	decl, ok := r.decls[l.Server+"/"+l.Tool]
	if !ok {
		decl = sinkclass.Default()
	}
	s, err := r.session(l.Generation)
	if err != nil {
		return nil, err
	}
	out := &replayed{line: l, legs: s.legs}
	args, err := SplitArgs(l.Args, decl, r.pol.hosts())
	if err != nil {
		reason := ReasonCheck
		if errors.Is(err, ErrAmbiguousAddress) {
			reason = ReasonAmbiguous
		}
		out.verdict = Verdict{Action: Refuse, Reason: reason, Detail: err.Error()}
		return out, nil
	}
	out.class = r.pol.classify(srv, decl, args.Address, r.now)
	out.bytes = args.PayloadBytes()
	if out.class == sinkclass.None || out.class == sinkclass.Internal {
		out.verdict = Decide(Facts{Class: out.class})
	} else {
		taint, err := leakguard.TaintChecker{Store: s.store, Limits: r.g.limits}.Check(ctx, s.scope, s.fp, args.Payload)
		if err != nil {
			return nil, err
		}
		f := Facts{Class: out.class, BothLegs: s.legs.Both(), Taint: taint, Cap: r.g.cap, PayloadBytes: out.bytes}
		if out.class != sinkclass.ExternalLow {
			out.verdict = Decide(f)
		} else {
			run := r.run(l.Run)
			st, err := cloneRunState(run.st)
			if err != nil {
				return nil, err
			}
			v, used := r.g.decideExternalLow(st, run.used, srv, decl, args, f)
			out.verdict = v
			if v.Action != Refuse {
				run.st, run.used = st, used
			}
		}
	}
	// A refused call never ran; an approved one did, and its result reaches the session
	// (RecordApproved), so the replay takes the recorded result of every call that was not refused.
	if out.verdict.Action != Refuse && len(l.Result) > 0 {
		legs := resultLegs(srv, decl)
		var texts []string
		if legs.PrivateData {
			texts = Strings(l.Result)
		}
		if err := r.record(ctx, s, legs, texts); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func cloneRunState(st *leakguard.RunState) (*leakguard.RunState, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	out := leakguard.NewRunState()
	return out, json.Unmarshal(b, out)
}

// replayAll replays a corpus and returns the decision on every call, in order.
func replayAll(ctx context.Context, lines []corpusLine, o replayOptions) ([]replayed, error) {
	r, err := newReplay(lines, o)
	if err != nil {
		return nil, err
	}
	var out []replayed
	for _, l := range lines {
		d, err := r.apply(ctx, l)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	return out, nil
}

// Thresholds is the sweep of the text-match threshold (ADR 0091 open questions: 24 by default, 12
// to 256).
var measureThresholds = []int{12, 16, 20, 24, 32, 48, 64, 128, 256}

// measurement holds the numbers the open questions of ADR 0091 ask for, from one corpus.
type measurement struct {
	Runs, RunsBothLegs               int
	Generations, GenerationsBothLegs int
	Classes                          map[sinkclass.Class]int
	// ApprovalCalls are external-high calls made while the session held both legs (the approval
	// branch), in ApprovalRuns runs.
	ApprovalCalls, ApprovalRuns int
	// LowAfterBoth is, per run that sent anything through external-low sinks after it held both
	// legs, the payload bytes it sent then (no cap applied: what the run tried to send).
	LowAfterBoth []int
	Cap          int
	RunsOverCap  int
	// Scope of the cap: generations with more than one run, their bytes after both legs summed over
	// the generation, and how many go over the cap per generation with no run over it per run.
	MultiRunGenerations       int
	GenerationBytes           []int
	GenerationsOnlyOverPerGen int
	// Matches by threshold: external calls whose payload matched private text, of External.
	Matches  map[int]int
	External int
}

// measure replays lines without a byte cap (to see what runs send) and at each threshold of the
// sweep, and returns the numbers of ADR 0091's open questions.
func measure(ctx context.Context, lines []corpusLine) (measurement, error) {
	m := measurement{Classes: map[sinkclass.Class]int{}, Matches: map[int]int{}, Cap: DefaultByteCap}
	calls, err := replayAll(ctx, lines, replayOptions{ByteCap: math.MaxInt32})
	if err != nil {
		return m, err
	}
	runGen := map[string]string{}
	var runOrder []string
	runBoth, genBoth, approvalRun := map[string]bool{}, map[string]bool{}, map[string]bool{}
	runLow, genLow, genRuns := map[string]int{}, map[string]int{}, map[string]map[string]bool{}
	for _, l := range lines {
		if l.Type != "call" && l.Type != "feed" {
			continue
		}
		if _, ok := runGen[l.Run]; !ok {
			runGen[l.Run] = l.Generation
			runOrder = append(runOrder, l.Run)
		}
		if genRuns[l.Generation] == nil {
			genRuns[l.Generation] = map[string]bool{}
		}
		genRuns[l.Generation][l.Run] = true
	}
	for _, c := range calls {
		run, gen := c.line.Run, c.line.Generation
		m.Classes[c.class]++
		if !c.legs.Both() {
			continue
		}
		runBoth[run], genBoth[gen] = true, true
		switch c.class {
		case sinkclass.ExternalHigh:
			m.ApprovalCalls++
			approvalRun[run] = true
		case sinkclass.ExternalLow:
			runLow[run] += c.bytes
			genLow[gen] += c.bytes
		}
	}
	// A run holds both legs when a call of it saw both; a run whose session got its second leg from
	// its last call never decided anything on both, and is not counted.
	m.Runs, m.Generations = len(runOrder), len(genRuns)
	m.RunsBothLegs, m.GenerationsBothLegs, m.ApprovalRuns = len(runBoth), len(genBoth), len(approvalRun)
	for _, run := range runOrder {
		if b, ok := runLow[run]; ok {
			m.LowAfterBoth = append(m.LowAfterBoth, b)
			if b > m.Cap {
				m.RunsOverCap++
			}
		}
	}
	gens := make([]string, 0, len(genRuns))
	for g := range genRuns {
		gens = append(gens, g)
	}
	slices.Sort(gens)
	for _, g := range gens {
		if len(genRuns[g]) < 2 {
			continue
		}
		m.MultiRunGenerations++
		m.GenerationBytes = append(m.GenerationBytes, genLow[g])
		anyRunOver := false
		for run := range genRuns[g] {
			anyRunOver = anyRunOver || runLow[run] > m.Cap
		}
		if genLow[g] > m.Cap && !anyRunOver {
			m.GenerationsOnlyOverPerGen++
		}
	}
	for _, t := range measureThresholds {
		calls, err := replayAll(ctx, lines, replayOptions{ByteCap: math.MaxInt32, Threshold: t})
		if err != nil {
			return m, err
		}
		ext := 0
		for _, c := range calls {
			if c.class != sinkclass.ExternalLow && c.class != sinkclass.ExternalHigh {
				continue
			}
			ext++
			if c.verdict.Match != nil {
				m.Matches[t]++
			}
		}
		m.External = ext
	}
	return m, nil
}

// quantile returns the q-quantile of xs (nearest rank), 0 for none.
func quantile(xs []int, q float64) int {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// report is the measurement as text. label says what the corpus is.
func (m measurement) report(label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Leak guard measurement, ADR 0091 open questions: %s\n", label)
	fmt.Fprintf(&b, "  corpus: %d runs in %d generations; calls by sink class: none %d, internal %d, external-low %d, external-high %d\n",
		m.Runs, m.Generations, m.Classes[sinkclass.None], m.Classes[sinkclass.Internal], m.Classes[sinkclass.ExternalLow], m.Classes[sinkclass.ExternalHigh])
	fmt.Fprintf(&b, "  1. byte cap (%d B per run): external-low payload bytes after both legs, %d runs: median %d, p90 %d, max %d; runs over the cap: %d\n",
		m.Cap, len(m.LowAfterBoth), quantile(m.LowAfterBoth, .5), quantile(m.LowAfterBoth, .9), quantile(m.LowAfterBoth, 1), m.RunsOverCap)
	fmt.Fprintf(&b, "  2. both legs: %d of %d runs and %d of %d generations decided a call on both legs; external-high calls on both legs (the approval branch): %d, in %d runs\n",
		m.RunsBothLegs, m.Runs, m.GenerationsBothLegs, m.Generations, m.ApprovalCalls, m.ApprovalRuns)
	fmt.Fprintf(&b, "  3. text match on %d external payloads (false alarms if the corpus is benign), by threshold:", m.External)
	for _, t := range measureThresholds {
		fmt.Fprintf(&b, " %d: %d", t, m.Matches[t])
	}
	fmt.Fprintf(&b, "\n  4. cap scope: %d generations with more than one run; bytes after both legs per generation: median %d, max %d; over the cap per generation but no run over it per run: %d\n",
		m.MultiRunGenerations, quantile(m.GenerationBytes, .5), quantile(m.GenerationBytes, 1), m.GenerationsOnlyOverPerGen)
	return b.String()
}
