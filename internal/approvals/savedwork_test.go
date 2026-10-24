// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/google/uuid"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/savedwork"
)

// savedFixture is what koinon-agentd leaves behind for a stuck sandbox: the run ended, sealed
// work, and a system-made approval request.
type savedFixture struct {
	*fixture
	work     savedwork.Work
	id       pgtype.UUID
	approval store.ApprovalRequest
	branch   string
}

func (f *fixture) saved(ctx context.Context, mutate func(*savedwork.Work)) *savedFixture {
	GinkgoHelper()
	base := f.gh.Snapshot("widgets").Refs["main"]
	w := savedwork.Work{
		Manifest: savedwork.Manifest{
			Repo: "acme/widgets", Base: base,
			Files: []savedwork.File{
				{Path: "src/new.go", Status: "A", Mode: "100644", Size: 14, Added: 1},
				{Path: "run.sh", Status: "A", Mode: "100755", Size: 10, Added: 1},
				{Path: "README.md", Status: "D"},
			},
		},
		Content: map[string][]byte{"src/new.go": []byte("package main\n\n"), "run.sh": []byte("echo hi\n\n\n")[:10]},
	}
	if mutate != nil {
		mutate(&w)
	}
	base = w.Manifest.Base
	// The run ended, as it does before the sandbox is recreated.
	_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: f.org, ID: f.run.ID, Epoch: f.run.Epoch, Outcome: store.RunStateSucceeded})
	Expect(err).NotTo(HaveOccurred())
	sf := &savedFixture{fixture: f, work: w, id: mustID(uniqUUID())}
	sf.branch = savedwork.BranchName(f.ticket.ID.String(), time.Now())
	env, err := savedwork.Seal(f.ring, f.org.String(), sf.id.String(), w)
	Expect(err).NotTo(HaveOccurred())
	envJSON, _ := json.Marshal(env)
	mj, _ := json.Marshal(w.Manifest)
	expires := pgtype.Timestamptz{Time: time.Now().Add(savedwork.Retention), Valid: true}
	_, err = q.InsertSavedWork(ctx, store.InsertSavedWorkParams{
		OrgID: f.org, ID: sf.id, TicketID: f.ticket.ID, RunID: f.run.ID, AgentID: f.agent.ID, Repo: "acme/widgets",
		BaseSha: base, Branch: sf.branch, Manifest: mj, SizeBytes: w.Manifest.Bytes(), Envelope: envJSON, ExpiresAt: expires,
	})
	Expect(err).NotTo(HaveOccurred())
	args := savedwork.PushArgsJSON(sf.id.String(), "acme/widgets", base, sf.branch)
	sf.approval, err = q.CreateSystemApprovalRequest(ctx, store.CreateSystemApprovalRequestParams{
		OrgID: f.org, TicketID: f.ticket.ID, RunID: f.run.ID, AgentID: f.agent.ID, Connector: "github", Tool: savedwork.ToolPush,
		Target: "github:acme/widgets", ArgsJson: args, ArgsSha256: sha(args), ExpiresAt: expires,
	})
	Expect(err).NotTo(HaveOccurred())
	return sf
}

var _ = Describe("Pushing saved work", func() {
	It("pushes koinon/wip/<ticket>-<time> only after an approval, with the saved files and the base as parent", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty(), "nothing reaches GitHub before the approval")
		Expect(f.approval.System).To(BeTrue())
		Expect(f.approval.Status).To(Equal(store.ApprovalStatusPending))

		_, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, "0000000000000000000000000000000000000000000000000000000000000000", "")
		Expect(err).To(MatchError(approvals.ErrHashMismatch))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())

		row, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "keep it")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)

		repo := f.gh.Snapshot("widgets")
		Expect(repo.Refs).To(HaveKey(f.branch))
		Expect(repo.Refs).To(HaveKey("main"))
		head := repo.Commits[repo.Refs[f.branch]]
		Expect(head.Parents).To(Equal([]string{f.work.Manifest.Base}))
		Expect(repo.Pulls).To(BeEmpty(), "a branch, no pull request")
		Expect(f.gh.Files("widgets", f.branch)).To(HaveKeyWithValue("src/new.go", []byte("package main\n\n")))
		Expect(f.gh.Files("widgets", f.branch)).NotTo(HaveKey("README.md"), "the deletion is part of the commit")
		Expect(f.gh.FileModes("widgets", f.branch)).To(HaveKeyWithValue("run.sh", "100755"))
		Expect(head.Message).To(And(ContainSubstring("Requested-by: Olive"), ContainSubstring("Approved-by: Adam"), ContainSubstring("Koinon-Approval: "+f.approval.ID.String())))
		Expect(repo.Refs["main"]).To(Equal(f.work.Manifest.Base), "the default branch is untouched")
		Expect(f.gh.Requests()).To(ContainElement(HaveField("Permissions", map[string]string{"metadata": "read", "contents": "write"})))

		var res map[string]any
		Expect(json.Unmarshal(row.Result, &res)).To(Succeed())
		Expect(res).To(HaveKeyWithValue("branch", f.branch))
		Expect(res["url"]).To(Equal("https://github.com/acme/widgets/tree/" + f.branch))

		By("the sealed files are dropped once pushed, and the run is not touched")
		sw, err := q.GetSavedWork(ctx, store.GetSavedWorkParams{OrgID: f.org, ID: f.id})
		Expect(err).NotTo(HaveOccurred())
		Expect(sw.Status).To(Equal("pushed"))
		Expect(sw.Envelope).To(BeNil())
		Expect(f.getRun(ctx).State).To(Equal(store.RunStateSucceeded))
		Expect(kinds(f.ticketEvents(ctx))).To(ContainElements(approvals.KindApproved, approvals.KindExecuted))
	})

	It("is idempotent: executing again creates nothing more", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		_, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		calls := f.gh.Snapshot("widgets").Calls
		again, err := f.svc.Execute(ctx, f.org, f.approval.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Status).To(Equal(store.ApprovalStatusExecuted))
		Expect(f.gh.Snapshot("widgets").Calls).To(Equal(calls))
	})

	It("refuses a repository the run's agent version is not bound to, and writes nothing", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, func(w *savedwork.Work) { w.Manifest.Repo = "acme/gadgets" })
		row, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("does not match"))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
	})

	It("never writes to an existing branch of someone else", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		f.gh.SetBranch("widgets", f.branch, "a human's commit", map[string]string{"x.txt": "x"})
		row, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("was not created for this approval"))
		Expect(f.gh.Files("widgets", f.branch)).To(HaveKeyWithValue("x.txt", []byte("x")))
		Expect(f.gh.Files("widgets", f.branch)).NotTo(HaveKey("src/new.go"))
	})

	It("fails when the base commit is not on GitHub", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, func(w *savedwork.Work) { w.Manifest.Base = "1111111111111111111111111111111111111111" })
		row, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("is not on GitHub"))
		Expect(f.gh.Snapshot("widgets").Refs).NotTo(HaveKey(f.branch))
	})

	It("denies without touching GitHub, and does not resume or cancel anything for the run", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		row, err := f.svc.Deny(ctx, f.org, f.approval.ID, f.admin, "not needed")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusDenied))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
		sw, err := q.GetSavedWork(ctx, store.GetSavedWorkParams{OrgID: f.org, ID: f.id})
		Expect(err).NotTo(HaveOccurred())
		Expect(sw.Status).To(Equal("saved"), "kept until it expires")
	})

	It("does not make the finished run's sweeper cancel the pending request", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		_, err := f.svc.Sweep(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.get(ctx, f.approval.ID.String()).Status).To(Equal(store.ApprovalStatusPending))
	})

	It("is not part of the run's outcomes: the agent is not told, a parked run does not wait", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		// Another request parks the run; the system request is made while it is parked.
		req := f.call(ctx, "create_pull_request", prArgs())
		f.park(ctx)
		base := f.gh.Snapshot("widgets").Refs["main"]
		args := savedwork.PushArgsJSON(uniqUUID(), "acme/widgets", base, "koinon/wip/x-1")
		_, err := q.CreateSystemApprovalRequest(ctx, store.CreateSystemApprovalRequestParams{
			OrgID: f.org, TicketID: f.ticket.ID, RunID: f.run.ID, AgentID: f.agent.ID, Connector: "github", Tool: savedwork.ToolPush,
			Target: "github:acme/widgets", ArgsJson: args, ArgsSha256: sha(args),
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		undelivered, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{OrgID: f.org, RunID: f.run.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(undelivered).To(HaveLen(1))
		Expect(undelivered[0].ID.String()).To(Equal(req.ID))
		// Deciding the agent's own request resumes the run although the system request is open.
		_, err = f.svc.Deny(ctx, f.org, mustID(req.ID), f.admin, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(f.getRun(ctx).ReadyAt.Valid).To(BeTrue(), "the park is resolved")
	})

	It("drops saved work after 14 days and expires its request", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		_, err := pool.Exec(ctx, `UPDATE saved_work SET expires_at = now() - interval '1 minute' WHERE id = $1`, f.id)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE approval_requests SET expires_at = now() - interval '1 minute' WHERE id = $1`, f.approval.ID)
		Expect(err).NotTo(HaveOccurred())
		res, err := f.svc.Sweep(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.SavedWorkExpired).To(Equal(1))
		sw, err := q.GetSavedWork(ctx, store.GetSavedWorkParams{OrgID: f.org, ID: f.id})
		Expect(err).NotTo(HaveOccurred())
		Expect(sw.Status).To(Equal("expired"))
		Expect(sw.Envelope).To(BeNil(), "the sealed files are gone")
		Expect(f.get(ctx, f.approval.ID.String()).Status).To(Equal(store.ApprovalStatusExpired))
	})

	It("will not execute expired work even if it was approved before", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		_, err := pool.Exec(ctx, `UPDATE saved_work SET expires_at = now() - interval '1 minute' WHERE id = $1`, f.id)
		Expect(err).NotTo(HaveOccurred())
		row, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("expired"))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
	})

	It("refuses an envelope moved to another row (the binding fails to open)", func(ctx SpecContext) {
		f := newFixture(ctx, "standard").saved(ctx, nil)
		other := f.saved2(ctx)
		_, err := pool.Exec(ctx, `UPDATE saved_work SET envelope = (SELECT envelope FROM saved_work WHERE id = $2) WHERE id = $1`, f.id, other)
		Expect(err).NotTo(HaveOccurred())
		row, err := f.svc.Approve(ctx, f.org, f.approval.ID, f.admin, f.approval.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("cannot be opened"))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
	})
})

// saved2 stores a second sealed work in the fixture's org and returns its ID.
func (sf *savedFixture) saved2(ctx context.Context) pgtype.UUID {
	GinkgoHelper()
	id := mustID(uniqUUID())
	env, err := savedwork.Seal(sf.ring, sf.org.String(), id.String(), sf.work)
	Expect(err).NotTo(HaveOccurred())
	envJSON, _ := json.Marshal(env)
	mj, _ := json.Marshal(sf.work.Manifest)
	_, err = q.InsertSavedWork(ctx, store.InsertSavedWorkParams{
		OrgID: sf.org, ID: id, TicketID: sf.ticket.ID, RunID: sf.run.ID, AgentID: sf.agent.ID, Repo: "acme/widgets",
		BaseSha: sf.work.Manifest.Base, Branch: "koinon/wip/other-1", Manifest: mj, SizeBytes: sf.work.Manifest.Bytes(), Envelope: envJSON,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	Expect(err).NotTo(HaveOccurred())
	return id
}

func uniqUUID() string { return uuid.NewString() }

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
