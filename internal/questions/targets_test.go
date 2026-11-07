// SPDX-License-Identifier: Apache-2.0

package questions_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// Targets and recipients (old issue 182; ADR 0065 amendment 2026-10-28).
var _ = Describe("a question to a role", func() {
	var (
		w     world
		admin pgtype.UUID
	)
	// addUser adds a user with a role in the org.
	addUser := func(ctx context.Context, role store.OrgRole) pgtype.UUID {
		GinkgoHelper()
		_, id, _ := pgtest.NewOrg(ctx, pool)
		_, err := pool.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', $3)`, w.org, id, role)
		Expect(err).NotTo(HaveOccurred())
		return id
	}
	// ask creates and opens a question to target and parks the run, as agentd's turn end does.
	ask := func(ctx context.Context, target questions.Target) store.Question {
		GinkgoHelper()
		var row store.Question
		Expect(tx(ctx, func(q *store.Queries) error {
			recipients, err := questions.Recipients(ctx, q, w.org, target)
			if err != nil {
				return err
			}
			row, _, err = questions.Create(ctx, q, questions.Ask{
				Org: w.org, Task: w.task, Run: w.run.ID, Agent: w.agent, Target: target, Recipients: recipients,
				Question: "Ship the report?", Choices: []string{"Yes", "No"}, Default: "No",
				ExpiresAt: time.Now().Add(time.Hour), RemindAt: time.Now().Add(30 * time.Minute),
			})
			if err != nil {
				return err
			}
			if _, err := questions.OpenForRun(ctx, q, w.org, w.run.ID, w.task); err != nil {
				return err
			}
			_, err = q.ParkRun(ctx, store.ParkRunParams{
				OrgID: w.org, ID: w.run.ID, Epoch: w.run.Epoch, ParkReason: store.RunParkReasonInputRequired,
				ParkExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
			})
			return err
		})).To(Succeed())
		return row
	}
	yes := questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "Yes"}
	respond := func(ctx context.Context, id, by pgtype.UUID) error {
		return tx(ctx, func(q *store.Queries) error {
			_, err := questions.Respond(ctx, q, w.org, id, by, yes)
			return err
		})
	}
	recipients := func(ctx context.Context, id pgtype.UUID) []pgtype.UUID {
		GinkgoHelper()
		ids, err := q.ListActiveQuestionRecipientIDs(ctx, store.ListActiveQuestionRecipientIDsParams{OrgID: w.org, QuestionID: id})
		Expect(err).NotTo(HaveOccurred())
		return ids
	}
	countOf := func(ctx context.Context, query string, args ...any) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, query, args...).Scan(&n)).To(Succeed())
		return n
	}

	BeforeEach(func(ctx SpecContext) {
		w = newWorld(ctx)
		admin = addUser(ctx, store.OrgRoleAdmin)
	})

	It("asks every admin and owner, and the first answer wins", func(ctx SpecContext) {
		row := ask(ctx, questions.DefaultTarget)
		Expect(recipients(ctx, row.ID)).To(ConsistOf(w.user, admin))
		Expect(countOf(ctx, `SELECT count(*) FROM question_recipients WHERE question_id = $1 AND waiting_since IS NOT NULL`, row.ID)).To(Equal(2))
		for _, u := range []pgtype.UUID{w.user, admin} {
			inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: w.org, UserID: u})
			Expect(err).NotTo(HaveOccurred())
			Expect(inbox).To(HaveLen(1))
			Expect(inbox[0].WaitingSince.Valid).To(BeTrue())
		}

		Expect(respond(ctx, row.ID, admin)).To(Succeed())
		Expect(respond(ctx, row.ID, w.user)).To(MatchError(questions.ErrNotOpen))
		got, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: row.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.AnsweredBy).To(Equal(admin))
		var payload []byte
		Expect(pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = $3`, w.org, w.task, questions.KindAnswered).Scan(&payload)).To(Succeed())
		var p map[string]any
		Expect(json.Unmarshal(payload, &p)).To(Succeed())
		Expect(p).To(HaveKey("answered_by_name"))
		Expect(countOf(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'question.answer' AND summary->'after'->>'targetKind' = 'role'`, w.org)).To(Equal(1))
	})

	It("refuses an answer from a member who is not a recipient", func(ctx SpecContext) {
		row := ask(ctx, questions.DefaultTarget)
		member := addUser(ctx, store.OrgRoleMember)
		Expect(respond(ctx, row.ID, member)).To(MatchError(questions.ErrNotAskedTo))
	})

	It("refuses an answer from a recipient who left the org or became a viewer", func(ctx SpecContext) {
		// A second owner, so that the first can become a viewer (an org keeps one manual owner).
		second := addUser(ctx, store.OrgRoleOwner)
		row := ask(ctx, questions.DefaultTarget)
		Expect(recipients(ctx, row.ID)).To(ConsistOf(w.user, admin, second))

		_, err := pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, w.org, admin)
		Expect(err).NotTo(HaveOccurred())
		Expect(respond(ctx, row.ID, admin)).To(MatchError(questions.ErrNotAskedTo))
		Expect(recipients(ctx, row.ID)).To(ConsistOf(w.user, second))

		_, err = pool.Exec(ctx, `UPDATE org_memberships SET role = 'viewer' WHERE org_id = $1 AND user_id = $2`, w.org, w.user)
		Expect(err).NotTo(HaveOccurred())
		Expect(respond(ctx, row.ID, w.user)).To(MatchError(questions.ErrNotAskedTo))
		Expect(recipients(ctx, row.ID)).To(ConsistOf(second))
	})

	It("lets exactly one of two concurrent answers win", func(ctx SpecContext) {
		row := ask(ctx, questions.DefaultTarget)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, u := range []pgtype.UUID{w.user, admin} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				errs[i] = respond(ctx, row.ID, u)
			}()
		}
		wg.Wait()
		var failed []error
		for _, err := range errs {
			if err != nil {
				failed = append(failed, err)
			}
		}
		Expect(failed).To(HaveLen(1))
		Expect(failed[0]).To(MatchError(questions.ErrNotOpen))
		Expect(countOf(ctx, `SELECT count(*) FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = $3`, w.org, w.task, questions.KindAnswered)).To(Equal(1))
	})

	It("gives an orphaned question to the admins", func(ctx SpecContext) {
		member := addUser(ctx, store.OrgRoleMember)
		row := ask(ctx, questions.Target{Kind: store.NotifyTargetKindUser, User: member})
		Expect(recipients(ctx, row.ID)).To(ConsistOf(member))
		_, err := pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, w.org, member)
		Expect(err).NotTo(HaveOccurred())

		before := time.Now()
		n, err := questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">=", 1))
		Expect(recipients(ctx, row.ID)).To(ConsistOf(w.user, admin))
		var since time.Time
		Expect(pool.QueryRow(ctx, `SELECT min(waiting_since) FROM question_recipients WHERE question_id = $1 AND user_id = ANY($2)`, row.ID, []pgtype.UUID{w.user, admin}).Scan(&since)).To(Succeed())
		Expect(since).To(BeTemporally(">=", before.Add(-time.Second)))
		for _, u := range []pgtype.UUID{w.user, admin} {
			Expect(countOf(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = $2 AND kind = $3`, w.org, u, questions.NotificationKind)).To(Equal(1))
		}
		Expect(countOf(ctx, `SELECT count(*) FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = $3`, w.org, w.task, questions.KindRetargeted)).To(Equal(1))
		// Who may answer changed, so the audit log says so: by the system, to whom and why.
		Expect(countOf(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'question.retarget' AND target_kind = 'question' AND target_id = $2 AND actor_kind = 'system'`, w.org, row.ID.String())).To(Equal(1))
		var after []byte
		Expect(pool.QueryRow(ctx, `SELECT summary->'after' FROM audit_events WHERE org_id = $1 AND action = 'question.retarget' AND target_id = $2`, w.org, row.ID.String()).Scan(&after)).To(Succeed())
		var a struct {
			ToLabel    string   `json:"toLabel"`
			Recipients []string `json:"recipients"`
			Reason     string   `json:"reason"`
			TaskID     string   `json:"taskId"`
		}
		Expect(json.Unmarshal(after, &a)).To(Succeed())
		Expect(a.ToLabel).To(Equal("the org's admins"))
		Expect(a.Recipients).To(ConsistOf(w.user.String(), admin.String()))
		Expect(a.Reason).NotTo(BeEmpty())
		Expect(a.TaskID).To(Equal(w.task.String()))

		_, err = questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(countOf(ctx, `SELECT count(*) FROM question_recipients WHERE question_id = $1 AND removed_at IS NULL`, row.ID)).To(Equal(3), "a second pass adds nobody")
		Expect(countOf(ctx, `SELECT count(*) FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = $3`, w.org, w.task, questions.KindRetargeted)).To(Equal(1))
		Expect(countOf(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'question.retarget'`, w.org)).To(Equal(1), "a pass that changes nothing records nothing")
	})

	It("retargets an orphaned question to the trigger's notify target, not to every admin", func(ctx SpecContext) {
		// The task's trigger notifies the org's owners; w.user is the owner, admin is not.
		_, err := pool.Exec(ctx, `UPDATE tasks SET notify_kind = 'role', notify_role = 'owner' WHERE org_id = $1 AND id = $2`, w.org, w.task)
		Expect(err).NotTo(HaveOccurred())
		member := addUser(ctx, store.OrgRoleMember)
		row := ask(ctx, questions.Target{Kind: store.NotifyTargetKindUser, User: member})
		_, err = pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, w.org, member)
		Expect(err).NotTo(HaveOccurred())

		_, err = questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(recipients(ctx, row.ID)).To(ConsistOf(w.user), "the owners only; the admin was not named")
		var after []byte
		Expect(pool.QueryRow(ctx, `SELECT summary->'after' FROM audit_events WHERE org_id = $1 AND action = 'question.retarget' AND target_id = $2`, w.org, row.ID.String()).Scan(&after)).To(Succeed())
		Expect(string(after)).To(ContainSubstring("the org's owners"))
		Expect(string(after)).NotTo(ContainSubstring(admin.String()))
		Expect(countOf(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = $2 AND kind = $3`, w.org, admin, questions.NotificationKind)).To(BeZero())
	})

	It("falls back to the admins when the trigger's notify person is gone too", func(ctx SpecContext) {
		member := addUser(ctx, store.OrgRoleMember)
		_, err := pool.Exec(ctx, `UPDATE tasks SET notify_kind = 'user', notify_user_id = $3 WHERE org_id = $1 AND id = $2`, w.org, w.task, member)
		Expect(err).NotTo(HaveOccurred())
		row := ask(ctx, questions.Target{Kind: store.NotifyTargetKindUser, User: member})
		_, err = pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, w.org, member)
		Expect(err).NotTo(HaveOccurred())

		_, err = questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(recipients(ctx, row.ID)).To(ConsistOf(w.user, admin))
		var after []byte
		Expect(pool.QueryRow(ctx, `SELECT summary->'after' FROM audit_events WHERE org_id = $1 AND action = 'question.retarget' AND target_id = $2`, w.org, row.ID.String()).Scan(&after)).To(Succeed())
		var a map[string]any
		Expect(json.Unmarshal(after, &a)).To(Succeed())
		Expect(a).To(HaveKeyWithValue("requestedTarget", HaveKeyWithValue("user", member.String())))
		Expect(a).To(HaveKeyWithValue("fallbackReason", ContainSubstring("no longer a member")))
		var payload []byte
		Expect(pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = $3`, w.org, w.task, questions.KindRetargeted).Scan(&payload)).To(Succeed())
		Expect(string(payload)).To(And(ContainSubstring("requestedTarget"), ContainSubstring("fallbackReason")))
	})

	It("does not widen a role nobody holds to the admins", func(ctx SpecContext) {
		// The only owner (w.user) is deleted, so nobody holds the role the trigger notifies.
		_, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, w.user)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE tasks SET notify_kind = 'role', notify_role = 'owner' WHERE org_id = $1 AND id = $2`, w.org, w.task)
		Expect(err).NotTo(HaveOccurred())
		member := addUser(ctx, store.OrgRoleMember)
		row := ask(ctx, questions.Target{Kind: store.NotifyTargetKindUser, User: member})
		_, err = pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, w.org, member)
		Expect(err).NotTo(HaveOccurred())

		n, err := questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		Expect(countOf(ctx, `SELECT count(*) FROM question_recipients WHERE question_id = $1`, row.ID)).To(Equal(1), "no new recipient")
		Expect(countOf(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'question.retarget'`, w.org)).To(BeZero())
	})

	It("gives nothing to a question that is closed while the sweep runs", func(ctx SpecContext) {
		member := addUser(ctx, store.OrgRoleMember)
		row := ask(ctx, questions.Target{Kind: store.NotifyTargetKindUser, User: member})
		_, err := pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, w.org, member)
		Expect(err).NotTo(HaveOccurred())

		// An expiry holds the question's row, uncommitted, while the sweep runs.
		resolving, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = questions.Expire(ctx, store.New(resolving), w.org, row.ID)
		Expect(err).NotTo(HaveOccurred())
		n, err := questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero(), "the locked question is skipped")
		Expect(resolving.Commit(ctx)).To(Succeed())

		Expect(countOf(ctx, `SELECT count(*) FROM question_recipients WHERE question_id = $1`, row.ID)).To(Equal(1), "only the original recipient")
		Expect(countOf(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'question.retarget'`, w.org)).To(BeZero())
		Expect(countOf(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND kind = $2`, w.org, questions.NotificationKind)).To(BeZero())
		n, err = questions.RetargetOrphaned(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero(), "and once closed it is not orphaned")
	})

	It("keeps the call hash of a user target", func(ctx SpecContext) {
		a := questions.Ask{
			Target: questions.Target{Kind: store.NotifyTargetKindUser, User: w.user}, Recipients: []pgtype.UUID{w.user},
			Question: "Post it?", Choices: []string{"a", "b"}, Draft: "d", Default: "a",
		}
		b, _ := json.Marshal([]any{w.user.String(), "Post it?", []string{"a", "b"}, "d", "a"})
		sum := sha256.Sum256(b)
		row, _ := func() (store.Question, bool) {
			var r store.Question
			var created bool
			Expect(tx(ctx, func(q *store.Queries) (err error) {
				a.Org, a.Task, a.Run, a.Agent = w.org, w.task, w.run.ID, w.agent
				r, created, err = questions.Create(ctx, q, a)
				return err
			})).To(Succeed())
			return r, created
		}()
		Expect(row.CallSha256).To(Equal(hex.EncodeToString(sum[:])))
	})

	It("does not restart the age on a reminder, and reminds every recipient", func(ctx SpecContext) {
		row := ask(ctx, questions.DefaultTarget)
		var before time.Time
		Expect(pool.QueryRow(ctx, `SELECT min(waiting_since) FROM question_recipients WHERE question_id = $1`, row.ID).Scan(&before)).To(Succeed())

		n := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(n, pool)
		res, err := questions.RunOrgTimers(ctx, pool, n, w.org, time.Now().Add(45*time.Minute))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Sent).To(Equal(1))
		var after time.Time
		Expect(pool.QueryRow(ctx, `SELECT min(waiting_since) FROM question_recipients WHERE question_id = $1`, row.ID).Scan(&after)).To(Succeed())
		Expect(after).To(BeTemporally("==", before))
		for _, u := range []pgtype.UUID{w.user, admin} {
			Expect(countOf(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = $2 AND dedupe_key = $3`, w.org, u, "question-reminder:"+row.ID.String())).To(Equal(1))
		}
	})

	It("notifies every recipient once", func(ctx SpecContext) {
		row := ask(ctx, questions.DefaultTarget)
		n := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(n, pool)
		for range 2 {
			_, err := questions.NotifyPending(ctx, pool, n)
			Expect(err).NotTo(HaveOccurred())
		}
		for _, u := range []pgtype.UUID{w.user, admin} {
			Expect(countOf(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = $2 AND dedupe_key = $3`, w.org, u, "question:"+row.ID.String())).To(Equal(1))
		}
	})

	Describe("Recipients", func() {
		It("includes admins and owners for the member role and excludes viewers and deleted users", func(ctx SpecContext) {
			member := addUser(ctx, store.OrgRoleMember)
			viewer := addUser(ctx, store.OrgRoleViewer)
			gone := addUser(ctx, store.OrgRoleMember)
			_, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, gone)
			Expect(err).NotTo(HaveOccurred())
			got, err := questions.Recipients(ctx, q, w.org, questions.Target{Kind: store.NotifyTargetKindRole, Role: store.OrgRoleMember})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(w.user, admin, member))
			Expect(got).NotTo(ContainElements(viewer, gone))
		})

		It("resolves a user who became a viewer to no one, and a channel to the admins", func(ctx SpecContext) {
			member := addUser(ctx, store.OrgRoleMember)
			_, err := pool.Exec(ctx, `UPDATE org_memberships SET role = 'viewer' WHERE org_id = $1 AND user_id = $2`, w.org, member)
			Expect(err).NotTo(HaveOccurred())
			got, err := questions.Recipients(ctx, q, w.org, questions.Target{Kind: store.NotifyTargetKindUser, User: member})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeEmpty())
			got, err = questions.Recipients(ctx, q, w.org, questions.Target{Kind: store.NotifyTargetKindChannel, Channel: "00000000-0000-7000-8000-00000000c001"})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(w.user, admin))
		})
	})
})
