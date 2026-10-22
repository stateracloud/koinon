// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
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

	"github.com/procoduck/koinon/internal/agentd"
	"github.com/procoduck/koinon/internal/db/migrations"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
)

// koinon-agentd driving the real OpenCode image: Postgres in a container, the flavor in the
// Docker stand-in for a sandbox, and Exec as `docker exec --interactive` in place of
// OpenShell's non-TTY exec.
var _ = Describe("koinon-agentd with the opencode flavor", Ordered, func() {
	var (
		db     *pgxpool.Pool
		q      *store.Queries
		gw     *gateway
		sb     *sandbox
		org    pgtype.UUID
		user   pgtype.UUID
		agent  store.Agent
		ticket store.Ticket
	)

	BeforeAll(func(ctx SpecContext) {
		if dockerErr != nil {
			Skip("Docker is not available: " + dockerErr.Error())
		}
		ensureImage(opencode)

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
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "OpenCode", Flavor: store.AgentFlavorOpencode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
			OrgID: org, AgentID: a.ID, CreatedBy: user,
			Config: []byte(`{"model":{"name":"` + contractModel + `"},"instructions":"` + instructionsMarker + `: you are a contract test agent."}`),
		})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		ticket, err = q.CreateTicket(ctx, store.CreateTicketParams{OrgID: org, Title: "Contract", AgentID: agent.ID, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())

		gw = newGateway(contractScript(opencode))
		sb = newSandbox(opencode.image(), "OPENAI_API_KEY="+fakeKey)
		bg := context.Background()
		DeferCleanup(func() { sb.remove(context.Background()) })
		sb.start(bg)

		p, err := agentd.New(db, &dockerBackend{sb: sb}, agentd.Config{
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
		// The fake model answers helloPrompt only with the instructions agentd wrote in the
		// system prompt, and the Koinon tool was there for the first request.
		Expect(text).To(ContainSubstring(helloReply))
		Expect(gw.authHeaders()).To(HaveEach("Bearer " + fakeKey))
		Expect(gw.modelBodies()).To(ContainElement(And(ContainSubstring(helloPrompt), ContainSubstring(opencode.mcpToolName))))
		Expect(sb.run(ctx, "cat", instructionsPath)).To(ContainSubstring(instructionsMarker))
	}, SpecTimeout(4*time.Minute))

	It("loads the session on the next run, after the sandbox restarted", func(ctx SpecContext) {
		sb.stop(ctx)
		sb.start(context.Background())
		r := runTicket(ctx, recallPrompt)
		Expect(r.State).To(Equal(store.RunStateSucceeded), r.OutcomeReason.String)
		// OpenCode announces its commands after a load, so run.session need not come first.
		Expect(eventsOf(ctx, r)).To(ContainElement(WithTransform(
			func(e store.RunEvent) string { return e.Kind + " " + string(e.Payload) },
			And(HavePrefix("run.session "), ContainSubstring(`"loaded": true`)), // jsonb's spacing
		)))
		for _, e := range eventsOf(ctx, r) {
			Expect(e.Kind).NotTo(Equal("acp.user_message_chunk"), "the replayed history is not recorded")
		}
		Expect(gw.modelBodies()).To(ContainElement(And(ContainSubstring(recallPrompt), ContainSubstring(helloReply))))
	}, SpecTimeout(4*time.Minute))

	It("rejects the harness's requests for paths outside the session directory", func(ctx SpecContext) {
		r := runTicket(ctx, readPrompt)
		var perms []store.RunEvent
		for _, e := range eventsOf(ctx, r) {
			if e.Kind == "acp.permission" {
				perms = append(perms, e)
			}
		}
		Expect(perms).NotTo(BeEmpty())
		GinkgoWriter.Printf("permission decisions: %s (run %s: %s)\n", perms[0].Payload, r.State, r.OutcomeReason.String)
		// OpenCode asks for paths outside the session directory with tool kind "read", which the
		// mapping allows; agentd rejects them for the path.
		Expect(string(perms[0].Payload)).To(ContainSubstring(`"verdict": "reject"`))
		Expect(string(perms[0].Payload)).To(ContainSubstring(`"outsideWorkspace": "` + opencode.outsidePath + `"`))
	}, SpecTimeout(4*time.Minute))
})

// dockerBackend is agentd's execution backend on the Docker stand-in sandbox. The sandbox is
// always ready; Exec is `docker exec --interactive`, a non-TTY exec like OpenShell's.
type dockerBackend struct{ sb *sandbox }

func (d *dockerBackend) GetSandbox(context.Context, string, string) (execution.Sandbox, error) {
	return execution.Sandbox{Phase: execution.PhaseReady}, nil
}

func (d *dockerBackend) StartSandbox(context.Context, string, string) (execution.Sandbox, error) {
	return execution.Sandbox{Phase: execution.PhaseReady}, nil
}

func (d *dockerBackend) Exec(ctx context.Context, _, _ string, req execution.ExecRequest) (execution.Process, error) {
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
		return nil, err
	}
	outR, outW := io.Pipe()
	c.Stdout = outW
	if req.Stderr != nil {
		c.Stderr = req.Stderr
	}
	if err := c.Start(); err != nil {
		cancel()
		return nil, err
	}
	p := &dockerProcess{stdin: stdin, stdout: outR, cancel: cancel, done: make(chan struct{})}
	go func() {
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
