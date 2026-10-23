// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// koinon-agentd driving each real flavor image: Postgres in a container, the flavor in the
// Docker stand-in for a sandbox, and Exec as `docker exec --interactive` in place of
// OpenShell's non-TTY exec. agentd picks the launch command from the agent's flavor.
var _ = func() bool {
	for _, f := range flavors {
		describeAgentd(f)
	}
	return true
}()

func describeAgentd(f flavor) bool {
	return Describe("koinon-agentd with the "+f.name+" flavor", Ordered, func() {
		var (
			db      *pgxpool.Pool
			q       *store.Queries
			gw      *gateway
			sb      *sandbox
			org     pgtype.UUID
			user    pgtype.UUID
			agent   store.Agent
			ticket  store.Ticket
			backend *dockerBackend
		)

		BeforeAll(func(ctx SpecContext) {
			if dockerErr != nil {
				Skip("Docker is not available: " + dockerErr.Error())
			}
			ensureImage(f)

			ctr, err := postgres.Run(ctx, "postgres:18-alpine", postgres.WithDatabase("koinon"), postgres.BasicWaitStrategies())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
			dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
			Expect(err).NotTo(HaveOccurred())
			db, err = pgxpool.New(ctx, dsn)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(db.Close)
			sqlDB := stdlib.OpenDBFromPool(db)
			DeferCleanup(sqlDB.Close)
			provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
			Expect(err).NotTo(HaveOccurred())
			_, err = provider.Up(ctx)
			Expect(err).NotTo(HaveOccurred())
			q = store.New(db)

			Expect(pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind) VALUES ('flavors', 'Flavors', 'personal') RETURNING id`).Scan(&org); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', 'flavors', $1) RETURNING id`, org).Scan(&user); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, org, user)
				return err
			})).To(Succeed())
			a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: f.name, Flavor: f.storeFlavor, CreatedBy: user})
			Expect(err).NotTo(HaveOccurred())
			v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
				OrgID: org, AgentID: a.ID, CreatedBy: user,
				Config: []byte(`{"model":"` + contractModel + `","instructions":"` + instructionsMarker + `: you are a contract test agent."}`),
			})
			Expect(err).NotTo(HaveOccurred())
			agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
			Expect(err).NotTo(HaveOccurred())
			// What koinon-reconciler records for a ready sandbox, which is what agentd reads.
			ws := execution.WorkspaceName(org.Bytes)
			_, err = q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{OrgID: org, Workspace: ws})
			Expect(err).NotTo(HaveOccurred())
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			_, err = q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
				OrgID: org, AgentID: agent.ID, Workspace: ws,
				SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
			})
			Expect(err).NotTo(HaveOccurred())
			// The home volume outlives the container's stop and start, so its generation is fixed.
			_, err = q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
				OrgID: org, AgentID: agent.ID, State: store.SandboxStateReady, HomeGeneration: "flavors-home",
			})
			Expect(err).NotTo(HaveOccurred())
			ticket, err = q.CreateTicket(ctx, store.CreateTicketParams{OrgID: org, Title: "Contract", AgentID: agent.ID, CreatedBy: user})
			Expect(err).NotTo(HaveOccurred())

			gw = newGateway(contractScript(f))
			sb = newSandbox(f.image(), "OPENAI_API_KEY="+fakeKey)
			bg := context.Background()
			DeferCleanup(func() { sb.remove(context.Background()) })
			sb.start(bg)

			backend = &dockerBackend{sb: sb}
			p, err := agentd.New(db, backend, agentd.Config{
				OwnerID: "agentd-flavors", Workers: 1, Lease: 10 * time.Second, Poll: 100 * time.Millisecond,
				GatewayURL: gw.url(""), Logger: newLogger(),
			})
			Expect(err).NotTo(HaveOccurred())
			pctx, cancel := context.WithCancel(bg)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = p.Run(pctx)
			}()
			DeferCleanup(func() {
				cancel()
				<-done
			})
		}, NodeTimeout(10*time.Minute))

		runTicket := func(ctx context.Context, text string) store.Run {
			_, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
				OrgID: org, TicketID: ticket.ID, Kind: "message", ActorKind: store.TicketActorKindUser,
				ActorUserID: user, Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)),
			})
			Expect(err).NotTo(HaveOccurred())
			r, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent.ID, TicketID: ticket.ID, MaxWallClockSeconds: 300, MaxTokens: 100_000})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() store.RunState {
				got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: r.ID})
				Expect(err).NotTo(HaveOccurred())
				return got.State
			}).WithTimeout(3 * time.Minute).WithPolling(250 * time.Millisecond).Should(BeElementOf(
				store.RunStateSucceeded, store.RunStateFailed, store.RunStateCanceled))
			got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: r.ID})
			Expect(err).NotTo(HaveOccurred())
			// The run is over once agentd has swept after it, too.
			Eventually(backend.settled).WithTimeout(time.Minute).Should(BeTrue(), "agentd swept after the run")
			return got
		}

		eventsOf := func(ctx context.Context, r store.Run) []store.RunEvent {
			evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: org, RunID: r.ID, MaxEvents: 1000})
			Expect(err).NotTo(HaveOccurred())
			return evs
		}

		It("runs a ticket's first turn on a new session", func(ctx SpecContext) {
			r := runTicket(ctx, helloPrompt)
			Expect(r.State).To(Equal(store.RunStateSucceeded), r.OutcomeReason.String)
			t, err := q.GetTicket(ctx, store.GetTicketParams{OrgID: org, ID: ticket.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(t.HarnessSessionID.String).NotTo(BeEmpty())
			evs := eventsOf(ctx, r)
			var text string
			for _, e := range evs {
				if e.Kind == "acp.agent_message_chunk" {
					var u struct {
						Content struct{ Text string } `json:"content"`
					}
					Expect(json.Unmarshal(e.Payload, &u)).To(Succeed())
					text += u.Content.Text
				}
			}
			// For a flavor with instructionsInSystem, the fake model answers helloPrompt only with
			// the instructions agentd wrote in the system prompt.
			Expect(text).To(ContainSubstring(helloReply))
			Expect(gw.authHeaders()).To(HaveEach("Bearer " + fakeKey))
			Expect(sb.run(ctx, "cat", instructionsPath)).To(ContainSubstring(instructionsMarker))
			// The first chat request of the turn already offers the Koinon tools: agentd waited for
			// OpenCode's ready command, and Hermes and pi connect their MCP server before the first
			// prompt (internal/agentd, readyCommand).
			// Side requests without tools (OpenCode's title generator) do not count.
			first := ""
			for _, b := range gw.modelBodies() {
				var req struct {
					Tools []json.RawMessage `json:"tools"`
				}
				Expect(json.Unmarshal([]byte(b), &req)).To(Succeed())
				if strings.Contains(b, helloPrompt) && len(req.Tools) > 0 {
					first = b
					break
				}
			}
			Expect(first).To(ContainSubstring(f.mcpToolName), "the first request offers the Koinon tools")
		}, SpecTimeout(4*time.Minute))

		It("loads the session on the next run, after the sandbox restarted", func(ctx SpecContext) {
			sb.stop(ctx)
			sb.start(context.Background())
			r := runTicket(ctx, recallPrompt)
			Expect(r.State).To(Equal(store.RunStateSucceeded), r.OutcomeReason.String)
			// A harness may announce its commands after a load, so run.session need not come first.
			Expect(eventsOf(ctx, r)).To(ContainElement(WithTransform(
				func(e store.RunEvent) string { return e.Kind + " " + string(e.Payload) },
				And(HavePrefix("run.session "), ContainSubstring(`"loaded": true`)), // jsonb's spacing
			)))
			for _, e := range eventsOf(ctx, r) {
				Expect(e.Kind).NotTo(Equal("acp.user_message_chunk"), "the replayed history is not recorded")
			}
			Expect(gw.modelBodies()).To(ContainElement(And(ContainSubstring(recallPrompt), ContainSubstring(helloReply))))
		}, SpecTimeout(4*time.Minute))

		It("ends what the agent left running before the next run, which keeps its tools", func(ctx SpecContext) {
			before := requestTools(gw)
			Expect(before).NotTo(BeEmpty())
			// A background job, as the agent's terminal tool could leave one, that keeps putting
			// its change to the home back (f.plant).
			step := ":"
			if f.plant != nil {
				step = f.plant(ctx, sb)
			}
			pid := strings.TrimSpace(sb.run(ctx, "sh", "-c", `nohup sh -c "$1" >/dev/null 2>&1 & echo $!`,
				"sh", "while :; do "+step+"; sleep 0.1; done"))
			Expect(sb.run(ctx, "cat", "/proc/"+pid+"/cmdline")).To(ContainSubstring("while"), "the job runs")

			r := runTicket(ctx, helloPrompt)
			Expect(r.State).To(Equal(store.RunStateSucceeded), r.OutcomeReason.String)
			Expect(requestTools(gw)).To(Equal(before), "the run has the same tools")
			Expect(sweptPids(eventsOf(ctx, r))).To(ContainElement(pid), "agentd ended the job before the harness started")
			expectOnlyMain(ctx, sb)
		}, SpecTimeout(4*time.Minute))

		It("replaces a directory the agent put at the instruction file's path, and runs", func(ctx SpecContext) {
			sb.run(ctx, "sh", "-c", `rm -rf "$1" && mkdir -p "$1/inner"`, "sh", instructionsPath)
			r := runTicket(ctx, helloPrompt)
			Expect(r.State).To(Equal(store.RunStateSucceeded), r.OutcomeReason.String)
			Expect(sb.run(ctx, "stat", "-c", "%F", instructionsPath)).To(Equal("regular file\n"))
			Expect(sb.run(ctx, "cat", instructionsPath)).To(ContainSubstring(instructionsMarker))
		}, SpecTimeout(4*time.Minute))

		It("answers the harness's permission requests from the tier mapping", func(ctx SpecContext) {
			if f.permissionCall.Name == "" {
				Skip(f.name + " sends no permission requests")
			}
			r := runTicket(ctx, permitPrompt)
			var perms []store.RunEvent
			for _, e := range eventsOf(ctx, r) {
				if e.Kind == "acp.permission" {
					perms = append(perms, e)
				}
			}
			Expect(perms).NotTo(BeEmpty())
			GinkgoWriter.Printf("permission decisions: %s (run %s: %s)\n", perms[0].Payload, r.State, r.OutcomeReason.String)
			Expect(string(perms[0].Payload)).To(ContainSubstring(`"verdict": "` + f.agentdVerdict + `"`))
			if f.agentdVerdict == "allow" {
				Expect(r.State).To(Equal(store.RunStateSucceeded), r.OutcomeReason.String)
			}
		}, SpecTimeout(4*time.Minute))
	})
}

// requestTools returns the names of the tools the last chat request that offered any offered,
// sorted.
func requestTools(gw *gateway) []string {
	GinkgoHelper()
	bodies := gw.modelBodies()
	for i := len(bodies) - 1; i >= 0; i-- {
		var req struct {
			Tools []struct {
				Function struct{ Name string }
			}
		}
		Expect(json.Unmarshal([]byte(bodies[i]), &req)).To(Succeed())
		if len(req.Tools) == 0 {
			continue
		}
		var names []string
		for _, t := range req.Tools {
			names = append(names, t.Function.Name)
		}
		slices.Sort(names)
		return names
	}
	return nil
}

// sweptPids returns the processes koinon-agentd recorded ending before the run's harness
// started.
func sweptPids(evs []store.RunEvent) []string {
	GinkgoHelper()
	var pids []string
	for _, e := range evs {
		if e.Kind == "run.sandbox_swept" {
			var p struct{ Pids []string }
			Expect(json.Unmarshal(e.Payload, &p)).To(Succeed())
			pids = append(pids, p.Pids...)
		}
	}
	return pids
}

// expectOnlyMain checks that nothing runs in sb but its main process (PID 1) and the exec
// that looks: koinon-agentd ended the run's harness and what it left. Zombies do not count,
// since the stand-in's PID 1 (sleep) reaps nothing.
func expectOnlyMain(ctx context.Context, sb *sandbox) {
	GinkgoHelper()
	Eventually(func() string {
		return sb.run(ctx, "sh", "-c", `for d in /proc/[0-9]*; do `+
			`grep -qE '^State:\s+[^Z]' "$d/status" 2>/dev/null && [ "${d#/proc/}" != $$ ] && echo "${d#/proc/} $(tr '\0' ' ' < "$d/cmdline")"; done; true`)
	}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Equal("1 /bin/sleep infinity \n"))
}

// dockerBackend is agentd's execution backend on the Docker stand-in sandbox, recorded as
// ready in agent_sandboxes; Exec is `docker exec --interactive`, a non-TTY exec like
// OpenShell's.
type dockerBackend struct {
	sb *sandbox

	mu       sync.Mutex
	inflight int
	last     []string // the command of the latest exec
}

// settled reports whether koinon-agentd is done with the sandbox after a run: no exec is
// running and the last one was the sweep it runs once the harness has ended.
func (d *dockerBackend) settled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inflight == 0 && len(d.last) > 0 && d.last[len(d.last)-1] == "sweep"
}

func (d *dockerBackend) Exec(ctx context.Context, _, _ string, req execution.ExecRequest) (execution.Process, error) {
	d.mu.Lock()
	d.inflight++
	d.last = req.Command
	d.mu.Unlock()
	done := func() {
		d.mu.Lock()
		d.inflight--
		d.mu.Unlock()
	}
	args := []string{"exec", "--interactive"}
	if req.Workdir != "" {
		args = append(args, "--workdir", req.Workdir)
	}
	args = append(args, d.sb.name)
	args = append(args, req.Command...)
	cctx, cancel := context.WithCancel(ctx)
	c := exec.CommandContext(cctx, "docker", args...) //nolint:gosec // test-controlled arguments
	stdin, err := c.StdinPipe()
	if err != nil {
		cancel()
		done()
		return nil, err
	}
	outR, outW := io.Pipe()
	c.Stdout = outW
	if req.Stderr != nil {
		c.Stderr = req.Stderr
	}
	if err := c.Start(); err != nil {
		cancel()
		done()
		return nil, err
	}
	p := &dockerProcess{stdin: stdin, stdout: outR, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer done()
		err := c.Wait()
		p.code = c.ProcessState.ExitCode()
		if p.code < 0 {
			p.err = err
		}
		_ = outW.Close()
		close(p.done)
	}()
	return p, nil
}

type dockerProcess struct {
	stdin  io.WriteCloser
	stdout io.Reader
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
	code   int
	err    error
}

func (p *dockerProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *dockerProcess) Stdout() io.Reader     { return p.stdout }

func (p *dockerProcess) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *dockerProcess) Close() error {
	p.once.Do(p.cancel)
	<-p.done
	return nil
}
