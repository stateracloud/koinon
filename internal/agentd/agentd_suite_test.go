// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/pricing"
)

var (
	db *pgxpool.Pool
	q  *store.Queries
)

func TestAgentd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "agentd")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	requireTestingTag()
	db = pgtest.Start(ctx)
	q = store.New(db)
}, NodeTimeout(3*time.Minute))

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

const (
	gatewayURL   = "http://gateway.koinon.test:8080"
	model        = "fake/model-1"
	instructions = "You help with the task."
	// instructionsFile is what agentd writes for instructions.
	instructionsFile = "/sandbox/.koinon/AGENTS.md"
)

// env is one org with an active agent, its sandbox in a fake OpenShell gateway whose exec
// runs the fake harness, and helpers to create tasks and runs. Pools scan every org, so
// an env stops its pools and cancels its unfinished runs when the spec ends.
type env struct {
	org, user pgtype.UUID
	agent     store.Agent
	gw        *fakegateway.Gateway
	backend   *openshell.Backend
	harness   *fakeHarness
	// backendOverride, if set, is the backend the pools use (a wrapper of backend).
	backendOverride agentd.Backend
	workspace       string
	sandbox         string
	files           *files
}

// files emulates the sandbox's files that agentd writes and reads with /bin/sh: the
// instruction file and the harness's link to it.
type files struct {
	mu     sync.Mutex
	data   map[string][]byte
	links  map[string]string
	writes int
	// failWrite makes writes exit 1, failLink makes restoring a link exit 1; onRead runs
	// before every read of the file, onLink before every check of a link.
	failWrite bool
	failLink  bool
	onRead    func(data map[string][]byte)
	onLink    func(links map[string]string)
	// sweep answers the sweep script: its output and exit code. Nil ends nothing.
	sweep  func() (string, int)
	sweeps int
	// procinfo answers the script that reads the processes the sweep could not end (its lines),
	// savework the script that reads the workspace's uncommitted work (its output and exit code).
	procinfo     func() string
	procinfoRuns int
	savework     func(dir string) (string, int)
	saveworkRuns int
	// tar answers the home checkpoint's tar execs (checkpoint_test.go); nil: no tar.
	tar func(args []string, stdin io.Reader, stdout, stderr io.Writer) int
}

// sweepCount is how many times agentd ran the sweep script.
func (f *files) sweepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

func (f *files) get(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.data[path]
	return b, ok
}

// sh handles agentd's /bin/sh -c SCRIPT sh ARGS... execs by script.
func (f *files) sh(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 3 && args[0] == "-c" && args[1] == agentd.SweepScript {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sweeps++
		if f.sweep == nil {
			_, _ = io.WriteString(stdout, "ended: none\n")
			return 0
		}
		out, code := f.sweep()
		if code != 0 {
			// Like sweep.sh: survivors are printed on stdout, the rest on stderr.
			for line := range strings.Lines(out) {
				if strings.HasPrefix(line, "survivors:") {
					_, _ = io.WriteString(stdout, line)
				} else {
					_, _ = io.WriteString(stderr, line)
				}
			}
		} else {
			_, _ = io.WriteString(stdout, out)
		}
		return code
	}
	if len(args) >= 3 && args[0] == "-c" && args[1] == agentd.ProcinfoScript {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.procinfoRuns++
		if f.procinfo != nil {
			_, _ = io.WriteString(stdout, f.procinfo())
		}
		return 0
	}
	if len(args) == 6 && args[0] == "-c" && args[1] == agentd.SaveworkScript {
		f.mu.Lock()
		f.saveworkRuns++
		fn := f.savework
		f.mu.Unlock() // the fake may be slow; other execs must not wait for it
		if fn == nil {
			return 0
		}
		out, code := fn(args[3])
		_, _ = io.WriteString(stdout, out)
		return code
	}
	if len(args) >= 4 && args[0] == "-c" && args[2] == "tar" && f.tar != nil {
		return f.tar(args[3:], stdin, stdout, stderr)
	}
	if len(args) >= 4 && args[0] == "-c" && args[1] == agentd.RealpathScript {
		// The sandbox has no links: every path is its own physical path.
		for _, p := range args[3:] {
			_, _ = fmt.Fprintln(stdout, p)
		}
		return 0
	}
	if len(args) < 4 || args[0] != "-c" {
		_, _ = fmt.Fprintf(stderr, "unexpected sh %q", args)
		return 2
	}
	in, _ := io.ReadAll(stdin)
	if strings.Contains(args[1], "koinon-gitconfig") || strings.Contains(args[1], "koinon-gitca") {
		// The rendered git configuration and CA file (gitconfig.go); the file is kept by path.
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(args[1], "mktemp"):
			f.data[args[4]] = in
		case len(args) == 5:
			delete(f.data, args[4])
		}
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch script := args[1]; {
	case strings.Contains(script, "mktemp") && len(args) == 5:
		f.writes++
		if f.failWrite {
			_, _ = io.WriteString(stderr, "mktemp: Read-only file system")
			return 1
		}
		f.data[args[4]] = in
		return 0
	case strings.Contains(script, "exec cat"):
		if f.onRead != nil {
			f.onRead(f.data)
		}
		b, ok := f.data[args[3]]
		if !ok {
			return 1
		}
		_, _ = stdout.Write(b)
		return 0
	case strings.Contains(script, "readlink") && len(args) == 5:
		if f.onLink != nil {
			f.onLink(f.links)
		}
		found, ok := f.links[args[3]]
		if ok && found == args[4] {
			return 0
		}
		if f.failLink {
			_, _ = io.WriteString(stderr, "rm: Read-only file system")
			return 1
		}
		f.links[args[3]] = args[4]
		_, _ = io.WriteString(stdout, found)
		return 3
	default:
		_, _ = fmt.Fprintf(stderr, "unexpected script %q", script)
		return 2
	}
}

func newEnv(ctx context.Context, harness string) *env {
	return newEnvWith(ctx, harness, nil)
}

// newEnvWith is newEnv with the agent version's config from configFor, which runs once the
// org and user exist.
func newEnvWith(ctx context.Context, harness string, configFor func(*env) string) *env {
	e := &env{}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Personal', 'personal') RETURNING id`,
			uniq("org")).Scan(&e.org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), e.org).Scan(&e.user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, e.org, e.user)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) {
		_, err := db.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'spec ended',
			finished_at = now(), ready_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL
			WHERE org_id = $1 AND state NOT IN ('succeeded', 'failed', 'canceled')`, e.org)
		Expect(err).NotTo(HaveOccurred())
		// Pools claim across every org in the shared database and each spec has its own fake
		// gateway: a sandbox row left ready, or a request left open, would be worked by a later
		// spec's pool against a gateway that lacks the sandbox and starve that spec's own.
		_, err = db.Exec(ctx, `UPDATE agent_sandboxes SET state = 'stopped' WHERE org_id = $1 AND state = 'ready'`, e.org)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE home_checkpoints SET status = 'failed', error = 'spec ended', finished_at = now()
			WHERE org_id = $1 AND status IN ('requested', 'running')`, e.org)
		Expect(err).NotTo(HaveOccurred())
	})

	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
		OrgID: e.org, DisplayName: uniq("agent"), Harness: harness, CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	config := `{"model":"` + model + `","instructions":"` + instructions + `"}`
	if configFor != nil {
		config = configFor(e)
	}
	version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: e.org, AgentID: agent.ID, Config: []byte(config), CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	e.agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
		OrgID: e.org, ID: agent.ID, VersionID: version.ID, ActivatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())

	e.gw = fakegateway.New()
	conn, err := e.gw.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(e.gw.Stop)
	DeferCleanup(conn.Close)
	e.backend = openshell.New(conn)
	e.workspace, e.sandbox = execution.WorkspaceName(e.org.Bytes), execution.AgentSandboxName(e.org.Bytes, e.agent.ID.Bytes)
	_, err = e.backend.EnsureWorkspace(ctx, e.workspace, nil)
	Expect(err).NotTo(HaveOccurred())
	_, err = e.backend.CreateSandbox(ctx, e.workspace, execution.SandboxSpec{Name: e.sandbox, Image: "ghcr.io/stateracloud/koinon/flavors/opencode:test"})
	Expect(err).NotTo(HaveOccurred())
	// What koinon-reconciler records for the sandbox, which is all agentd reads about it.
	_, err = q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{OrgID: e.org, Workspace: e.workspace})
	Expect(err).NotTo(HaveOccurred())
	names := execution.AgentObjectNames(e.org.Bytes, e.agent.ID.Bytes)
	_, err = q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
		OrgID: e.org, AgentID: e.agent.ID, Workspace: e.workspace,
		SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
	})
	Expect(err).NotTo(HaveOccurred())
	e.recordSandbox(ctx, store.SandboxStateReady, "")

	e.harness = newHarness()
	for _, script := range []string{"/usr/local/bin/koinon-opencode"} {
		e.gw.Handle(script, e.harness.command)
	}
	e.gw.Handle("/bin/mkdir", func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { return 0 })
	e.files = &files{data: map[string][]byte{}, links: map[string]string{}}
	e.gw.Handle("/bin/sh", e.files.sh)
	return e
}

// recordSandbox writes the sandbox state as koinon-reconciler would, with the sandbox's
// OpenShell ID as its home generation.
func (e *env) recordSandbox(ctx context.Context, state store.SandboxState, message string) {
	e.recordSandboxWith(ctx, state, "", message)
}

// recordSandboxWith is recordSandbox with the reason of the condition that is not true, as the
// reconciler records it (ErrImagePull, Unschedulable).
func (e *env) recordSandboxWith(ctx context.Context, state store.SandboxState, reason, message string) {
	_, err := q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
		OrgID: e.org, AgentID: e.agent.ID, State: state, Phase: "", Message: message, Reason: reason,
		HomeGeneration: e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId(),
	})
	Expect(err).NotTo(HaveOccurred())
}

// recreateSandbox replaces the sandbox, and so its home volume, as the reconciler does on a
// spec change, and records the new one ready.
func (e *env) recreateSandbox(ctx context.Context) {
	_, derr := e.backend.DeleteSandbox(ctx, e.workspace, e.sandbox)
	Expect(derr).To(Succeed())
	_, err := e.backend.CreateSandbox(ctx, e.workspace, execution.SandboxSpec{Name: e.sandbox, Image: "koinon/flavor-opencode:dev"})
	Expect(err).NotTo(HaveOccurred())
	e.recordSandbox(ctx, store.SandboxStateReady, "")
}

// task creates a task for the agent with one user message.
func (e *env) task(ctx context.Context, message string) store.Task {
	t, err := q.CreateTask(ctx, store.CreateTaskParams{
		OrgID: e.org, Title: uniq("task"), AgentID: e.agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	e.message(ctx, t, message)
	return t
}

func (e *env) message(ctx context.Context, t store.Task, text string) {
	_, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: e.org, TaskID: t.ID, Kind: "message", ActorKind: store.TaskActorKindUser,
		ActorUserID: e.user, Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)),
	})
	Expect(err).NotTo(HaveOccurred())
}

func (e *env) enqueue(ctx context.Context, t store.Task) store.Run {
	r, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: 1800,
	})
	Expect(err).NotTo(HaveOccurred())
	return r
}

// pool starts a pool on the env's gateway and stops it when the spec ends. The returned
// function stops it early, like a killed pod when the pool keeps its leases.
func (e *env) pool(name string, mutate ...func(*agentd.Config)) (stop func()) {
	return e.poolOn(db, name, mutate...)
}

// poolOn is pool on its own connection pool, like a separate agentd process on the same Postgres.
func (e *env) poolOn(conns *pgxpool.Pool, name string, mutate ...func(*agentd.Config)) (stop func()) {
	return e.poolRunning(conns, name, (*agentd.Pool).Run, mutate...)
}

// heldPool is a pool in restore-hold mode (issue #462): it runs RunHeld, not Run.
func (e *env) heldPool(name string, mutate ...func(*agentd.Config)) (stop func()) {
	return e.poolRunning(db, name, (*agentd.Pool).RunHeld, mutate...)
}

// poolRunning starts a pool whose loop is run.
func (e *env) poolRunning(conns *pgxpool.Pool, name string, run func(*agentd.Pool, context.Context) error, mutate ...func(*agentd.Config)) (stop func()) {
	cfg := agentd.Config{
		Prices:         prices.NewResolver(pricing.Embedded()),
		OwnerID:        name,
		Workers:        2,
		Lease:          racescale.D(2 * time.Second),
		Heartbeat:      racescale.D(200 * time.Millisecond), // scaled with Lease: the ratio must hold
		Poll:           50 * time.Millisecond,
		GatewayURL:     gatewayURL,
		Logger:         slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})).With("pool", name),
		CancelGrace:    racescale.D(2 * time.Second),
		SandboxTimeout: racescale.D(10 * time.Second),
		ReadyTimeout:   racescale.D(2 * time.Second),
		ReadySettle:    10 * time.Millisecond,
		// Specs that fail a run with an authentication error have no access-log record to wait for.
		GatewayRecordWait: racescale.D(300 * time.Millisecond),
		// A short drain, so a spec that ends with a turn in progress does not wait out the default
		// 5 minutes; the drain specs set their own.
		DrainDeadline: 200 * time.Millisecond,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	var be agentd.Backend = e.backend
	if e.backendOverride != nil {
		be = e.backendOverride
	}
	p, err := agentd.New(conns, be, cfg)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(run(p, ctx)).To(Succeed())
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			Eventually(done).WithTimeout(racescale.D(10 * time.Second)).Should(BeClosed())
		}
	}
	DeferCleanup(stop)
	return stop
}

func getRun(ctx context.Context, r store.Run) store.Run {
	got, err := q.GetRun(ctx, store.GetRunParams{OrgID: r.OrgID, ID: r.ID})
	Expect(err).NotTo(HaveOccurred())
	return got
}

func runState(ctx context.Context, r store.Run) func() store.RunState {
	return func() store.RunState { return getRun(ctx, r).State }
}

func events(ctx context.Context, r store.Run) []store.RunEvent {
	evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: r.OrgID, RunID: r.ID, MaxEvents: 1000})
	Expect(err).NotTo(HaveOccurred())
	return evs
}

func kinds(evs []store.RunEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func kindsOf(evs []store.TaskEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func taskEvents(ctx context.Context, e *env, t store.Task) []store.TaskEvent {
	evs, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: e.org, TaskID: t.ID, MaxEvents: 100})
	Expect(err).NotTo(HaveOccurred())
	return evs
}

// fakeClock is the pool's clock in tests that pass the sandbox wait without waiting for it.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	reads int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.t
}

// awaitWait blocks until waitSandbox has gone round its loop once: it reads the clock for the start
// of the wait, then for the nudge and the deadline checks, which come after it has read the org's
// queue wait and set its deadline. Advancing earlier would move the clock before the wait began.
func (c *fakeClock) awaitWait() {
	GinkgoHelper()
	Eventually(func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.reads
	}).WithTimeout(racescale.D(10 * time.Second)).Should(BeNumerically(">=", 3))
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
