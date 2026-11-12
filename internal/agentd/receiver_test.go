// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/savedwork"
)

// koinon-api's side of saved work (package savedwork): the credential, the run, the bundle.
var _ = Describe("the saved-work receiver", func() {
	var (
		e      *env
		url    string
		key    []byte
		run    store.Run
		epoch  int64
		packed []byte
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnvWith(ctx, harness.OpenCode, bound)
		url = receiverURL()
		key = uploadKey
		t := e.task(ctx, "hello")
		e.enqueue(ctx, t)
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "x", LeaseSeconds: 600})
		Expect(err).NotTo(HaveOccurred())
		run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		epoch = run.Epoch
		packed, err = savedwork.Pack(savedwork.Work{
			Manifest: savedwork.Manifest{Repo: "acme/widgets", Base: baseSHA, Files: []savedwork.File{{Path: "a.txt", Status: "A", Mode: "100644", Size: 2, Added: 1}}},
			Content:  map[string][]byte{"a.txt": []byte("x\n")},
		})
		Expect(err).NotTo(HaveOccurred())
	})
	upload := func(ctx SpecContext, mutate func(org, run string, epoch int64, k []byte, body []byte) (string, string, int64, []byte)) (savedwork.UploadResult, error) {
		org, runID, ep, k, body := e.org.String(), run.ID.String(), epoch, key, packed
		if mutate != nil {
			org, runID, ep, body = mutate(org, runID, ep, k, body)
		}
		return savedwork.Upload(ctx, http.DefaultClient, url, k, org, runID, ep, 1, body)
	}

	It("stores a good bundle sealed, with its approval, for the run that owns the sandbox", func(ctx SpecContext) {
		res, err := upload(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Branch).To(HavePrefix("koinon/wip/"))
		var envelope string
		Expect(db.QueryRow(ctx, `SELECT envelope::text FROM saved_work WHERE id = $1`, res.SavedWorkID).Scan(&envelope)).To(Succeed())
		Expect(envelope).To(ContainSubstring("ciphertext"))
		Expect(envelope).NotTo(ContainSubstring("a.txt"))
		var args string
		Expect(db.QueryRow(ctx, `SELECT args_json FROM approval_requests WHERE id = $1`, res.ApprovalID).Scan(&args)).To(Succeed())
		a, err := savedwork.DecodePushArgs(args)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.BundleSHA256).To(Equal(savedwork.BundleHash(packed)), "the approval binds to the content")
	})

	DescribeTable("refuses",
		func(ctx SpecContext, mutate func(org, run string, epoch int64, k []byte, body []byte) (string, string, int64, []byte), want string) {
			_, err := upload(ctx, mutate)
			Expect(err).To(MatchError(ContainSubstring(want)))
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM saved_work WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		},
		Entry("another run's credential", func(org, run string, epoch int64, _ []byte, body []byte) (string, string, int64, []byte) {
			return org, "019a0000-0000-7000-8000-000000000000", epoch, body
		}, "422"),
		Entry("a stale epoch", func(org, run string, epoch int64, _ []byte, body []byte) (string, string, int64, []byte) {
			return org, run, epoch + 1, body
		}, "does not own its sandbox"),
		Entry("a bundle that is not a bundle", func(org, run string, epoch int64, _ []byte, _ []byte) (string, string, int64, []byte) {
			return org, run, epoch, []byte("not gzip")
		}, "422"),
	)

	It("refuses a wrong key, an expired token and a repository the agent is not bound to", func(ctx SpecContext) {
		_, err := savedwork.Upload(ctx, http.DefaultClient, url, bytes.Repeat([]byte("z"), 32), e.org.String(), run.ID.String(), epoch, 1, packed)
		Expect(err).To(MatchError(ContainSubstring("401")))
		now := time.Now()
		tok := func(exp int64, n int, sha, nonce string) string {
			return savedwork.Token(key, "o", "r", 1, exp, n, sha, nonce)
		}
		sha, nonce := strings.Repeat("a", 64), strings.Repeat("b", 32)
		ok := now.Add(time.Minute).Unix()
		Expect(savedwork.ValidToken(key, tok(ok, 1, sha, nonce), "o", "r", 1, ok, 1, sha, nonce, now)).To(BeTrue())
		past := now.Add(-time.Minute).Unix()
		Expect(savedwork.ValidToken(key, tok(past, 1, sha, nonce), "o", "r", 1, past, 1, sha, nonce, now)).To(BeFalse(), "expired")
		far := now.Add(savedwork.TokenLifetime + time.Minute).Unix()
		Expect(savedwork.ValidToken(key, tok(far, 1, sha, nonce), "o", "r", 1, far, 1, sha, nonce, now)).To(BeFalse(), "beyond one lifetime")
		Expect(savedwork.ValidToken(key, tok(ok, 1, sha, nonce), "o", "r2", 1, ok, 1, sha, nonce, now)).To(BeFalse(), "another run")
		Expect(savedwork.ValidToken(key, tok(ok, 1, sha, nonce), "o", "r", 1, ok, 2, sha, nonce, now)).To(BeFalse(), "another repository number")
		Expect(savedwork.ValidToken(key, tok(ok, 1, sha, nonce), "o", "r", 1, ok, 1, strings.Repeat("c", 64), nonce, now)).To(BeFalse(), "another body")
		other, err := savedwork.Pack(savedwork.Work{
			Manifest: savedwork.Manifest{Repo: "evil/repo", Base: baseSHA, Files: []savedwork.File{{Path: "a", Status: "A", Mode: "100644", Size: 1}}},
			Content:  map[string][]byte{"a": []byte("x")},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = savedwork.Upload(ctx, http.DefaultClient, url, key, e.org.String(), run.ID.String(), epoch, 1, other)
		Expect(err).To(MatchError(ContainSubstring("not bound")))
	})

	It("refuses a replayed request and a swapped body", func(ctx SpecContext) {
		do := func(body []byte, bodySHA, nonce string, exp int64, token string) int {
			q := fmt.Sprintf("?org=%s&run=%s&epoch=%d&exp=%d&n=1", e.org, run.ID, epoch, exp)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+savedwork.UploadPath+q, bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set(savedwork.NonceHeader, nonce)
			req.Header.Set(savedwork.BodyHashHeader, bodySHA)
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = res.Body.Close()
			return res.StatusCode
		}
		exp := time.Now().Add(time.Minute).Unix()
		sha := savedwork.BundleHash(packed)
		nonce := strings.Repeat("1", 32)
		token := savedwork.Token(key, e.org.String(), run.ID.String(), epoch, exp, 1, sha, nonce)
		Expect(do(packed, sha, nonce, exp, token)).To(Equal(http.StatusOK))
		Expect(do(packed, sha, nonce, exp, token)).To(Equal(http.StatusConflict), "the same request twice")

		By("a body other than the one the credential was made for")
		other := append(append([]byte(nil), packed...), 0)
		nonce2 := strings.Repeat("2", 32)
		token2 := savedwork.Token(key, e.org.String(), run.ID.String(), epoch, exp, 1, sha, nonce2)
		Expect(do(other, sha, nonce2, exp, token2)).To(Equal(http.StatusUnauthorized))
		Expect(do(other, savedwork.BundleHash(other), nonce2, exp, token2)).To(Equal(http.StatusUnauthorized), "the header is covered too")
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM saved_work WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("is not open to a plain GET and reads no more than the cap", func() {
		srv := httptest.NewServer(http.NewServeMux())
		defer srv.Close()
		res, err := http.Get(url + savedwork.UploadPath)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = res.Body.Close() }()
		Expect(res.StatusCode).To(Equal(http.StatusMethodNotAllowed))
	})
})

var _ = Describe("what the sandbox reports", func() {
	It("is capped to 20 notes of 200 bytes", func() {
		var notes []string
		for range 50 {
			notes = append(notes, strings.Repeat("é", 300)+"\nsecond line")
		}
		out := agentd.CapNotes(notes)
		Expect(out).To(HaveLen(21))
		Expect(out[20]).To(Equal("and 30 more"))
		for _, n := range out[:20] {
			Expect(len(n)).To(BeNumerically("<=", savedwork.MaxNoteBytes))
			Expect(n).NotTo(ContainSubstring("\n"))
		}
	})

	It("must have a newline after every file's content", func() {
		_, _, err := agentd.ParseSaveOutput([]byte("R a/b " + baseSHA + "\nF A 100644 2 1 0 0 x\nabXX\nE\n"))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("the summarizing mark", func() {
	It("does not outlive its owner: a takeover or a new claim clears it", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "a", LeaseSeconds: 30})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(r.ID))
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		n, err := q.SetRunSummarizing(ctx, store.SetRunSummarizingParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch, Summarizing: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(1))
		_, err = q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).To(HaveOccurred(), "tools are off while it is set")

		_, err = db.Exec(ctx, `UPDATE runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, r.ID)
		Expect(err).NotTo(HaveOccurred())
		taken, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: e.org, OwnerID: "b", LeaseSeconds: 30})
		Expect(err).NotTo(HaveOccurred())
		Expect(taken.Summarizing).To(BeFalse())
		_, err = q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred(), "the new owner's tools work")

		By("the old owner can no longer set it")
		n, err = q.SetRunSummarizing(ctx, store.SetRunSummarizingParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch, Summarizing: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
	})
})

var _ = Describe("an unhealthy agent's parked run", func() {
	It("stays parked when its approval is decided, and is claimed once the agent is resumed", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "a", LeaseSeconds: 30})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ParkRun(ctx, store.ParkRunParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch, ParkReason: store.RunParkReasonApproval})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: e.org, ID: e.agent.ID, Reason: "stuck"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: e.org, ID: r.ID})
		Expect(err).NotTo(HaveOccurred())

		_, err = q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "b", LeaseSeconds: 30})
		Expect(err).To(HaveOccurred(), "nothing is claimed")
		Expect(getRun(ctx, r).State).To(Equal(store.RunStateParked))

		_, err = q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: e.org, ID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		again, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "b", LeaseSeconds: 30})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ID).To(Equal(r.ID))
	})
})
