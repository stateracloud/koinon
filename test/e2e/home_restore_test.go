// SPDX-License-Identifier: Apache-2.0

package e2e_test

// The home restore drill on the Substrate leg (issue #1090 part 3, S5). test/e2e/run.sh runs it after the
// quickstart pass when DRIVER=substrate and BACKUP_RESTORE=1 (label home-restore); the quickstart pass leaves it
// out (label restore). It is the kind form of the home drill on the fake (internal/execution/substrate/
// home_drill_test.go) and the single-install form of the archive check of the ADR 0109 drill
// (test/drill/drill_test.go), through Koinon's own path and nothing else:
//
//  1. turn home checkpoints on (an S3 server in the fakes namespace, helm upgrade with a short idle timeout and
//     archive delay);
//  2. a task writes a marker file into the agent's home (/sandbox, the actor's durable directory);
//  3. the sandbox goes idle: the reconciler takes the stop checkpoint (kopia, with a content digest), suspends the
//     actor, and archives it (DeleteActor: the durable directory goes with it, so the home is lost);
//  4. with every recorded digest of the agent's checkpoints made wrong, the next message starts a run whose
//     restore is refused (checkpoint.ErrDigestMismatch) before the harness starts;
//  5. with the digests put back, the next message starts a run that restores the home through the Substrate
//     upload path (tar on exec stdin, #1478), verified against the digest, and loads the task's session: the run
//     reads the marker.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/test/drill"
)

const (
	// homeDrillModel is declared by kind/model-provider.yaml and scripted by test/fakemodel/scripts/home-drill.yaml.
	homeDrillModel = "home-drill"
	// homeDrillRun is the value of the drill's run label on the object store (no sweep: the kind cluster goes).
	homeDrillRun = "e2e-home"
	// The short timers of the drill: the sandbox stops 2 minutes after its last run, and is archived 1 minute
	// after its stop checkpoint is complete (the source of the ADR 0109 drill uses the same).
	homeDrillIdle    = "2m"
	homeDrillArchive = "1m"
	// wrongDigest is a well-formed digest that no home has.
	wrongDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

var _ = Describe("the home restore drill on Substrate", Label("restore", "home-restore"), func() {
	It("restores an archived agent's home from its checkpoint, only with the recorded digest", NodeTimeout(30*time.Minute), func(ctx SpecContext) {
		kubectl, helm, chart := os.Getenv("KOINON_E2E_KUBECTL"), os.Getenv("KOINON_E2E_HELM"), os.Getenv("KOINON_E2E_CHART")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL (a kubectl bound to the e2e cluster's context)")
		Expect(helm).NotTo(BeEmpty(), "KOINON_E2E_HELM (a helm bound to the e2e cluster's context)")
		Expect(chart).NotTo(BeEmpty(), "KOINON_E2E_CHART (the chart under test)")
		fakes := os.Getenv("KOINON_E2E_FAKES")
		if fakes == "" {
			fakes = "koinon-e2e-fakes"
		}
		run := func(name string, args ...string) string {
			GinkgoHelper()
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "%s %s: %s", name, strings.Join(args, " "), out)
			return string(out)
		}
		apply := func(manifest string) {
			GinkgoHelper()
			cmd := exec.CommandContext(ctx, kubectl, "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
		}
		sql := func(query string) []string {
			GinkgoHelper()
			var rows []string
			Eventually(func(g Gomega) { rows = psqlPrimary(ctx, g, kubectl, query) }).
				WithContext(ctx).WithTimeout(time.Minute).WithPolling(5 * time.Second).Should(Succeed())
			return rows
		}
		one := func(query string) string {
			GinkgoHelper()
			rows := sql(query)
			if len(rows) == 0 {
				return ""
			}
			return rows[0]
		}

		By("an S3 server for home checkpoints, and the bucket (the object store of the ADR 0109 drill)")
		top, err := drill.LoadTopology("../drill/topology/kind.yaml", homeDrillRun)
		Expect(err).NotTo(HaveOccurred())
		apply(drill.ObjectStoreManifest(fakes, homeDrillRun, top.ObjectStoreImage))
		run(kubectl, "-n", fakes, "rollout", "status", "deploy/"+drill.ObjectStoreName, "--timeout=5m")
		s3 := portForwardIn(ctx, kubectl, fakes, "svc/"+drill.ObjectStoreName, fmt.Sprint(drill.ObjectStorePort))
		mc, err := minio.New(s3, &minio.Options{Creds: credentials.NewStaticV4(drill.ObjectStoreAccessKey, drill.ObjectStoreSecretKey, "")})
		Expect(err).NotTo(HaveOccurred())
		// SeaweedFS's S3 gateway can be slow to accept the first call.
		Eventually(func() error {
			err := mc.MakeBucket(ctx, drill.CheckpointBucket, minio.MakeBucketOptions{Region: drill.ObjectStoreRegion})
			if resp := minio.ToErrorResponse(err); resp.Code == "BucketAlreadyOwnedByYou" || resp.Code == "BucketAlreadyExists" {
				return nil
			}
			return err
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("home checkpoints on, with a short idle timeout and archive delay (helm upgrade --reuse-values)")
		apply(drill.CheckpointSecretManifest("koinon", homeDrillRun, fakes))
		run(helm, "upgrade", "koinon", chart, "--namespace", "koinon", "--reuse-values",
			"--set", "checkpoints.enabled=true",
			"--set", "checkpoints.s3.existingSecret="+drill.CheckpointSecretName,
			"--set", "checkpoints.s3.bucket="+drill.CheckpointBucket,
			"--set", "reconciler.idleTimeout="+homeDrillIdle,
			"--set", "reconciler.archiveAfter="+homeDrillArchive,
			"--wait", "--timeout", "10m")

		// The upgrade replaced the koinon-api pod, so run.sh's forward does not reach it: a new one.
		c, org := storiesSignIn(ctx, "http://"+portForward(ctx, kubectl, "svc/koinon-api", "8080"))
		tok, err := c.Tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
			OrgId: org, Name: "e2e-home-restore", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
		})
		Expect(err).NotTo(HaveOccurred())
		// The scripted model has no price of its own; a model is served only with one (SetAgentModel).
		code, body, err := c.Admin(ctx, tok.GetSecret(), "POST", "/prices", `{"name":"fake-home-drill","provider":"openai","match":{"glob":"home-drill*"},`+
			`"prices":{"input":"0.30","output":"1.20"},"maxOutput":4096}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Or(Equal(201), Equal(409)), string(body))

		By("an agent from the web-researcher template (no grants) on the scripted model")
		ts, err := c.Templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		tmpl := ""
		for _, t := range ts.GetTemplates() {
			if t.GetSlug() == storiesTemplate {
				tmpl = t.GetId()
			}
		}
		Expect(tmpl).NotTo(BeEmpty(), "the %s template", storiesTemplate)
		made, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{OrgId: org, TemplateId: tmpl, DisplayName: "Home drill agent"})
		Expect(err).NotTo(HaveOccurred())
		agent := made.GetAgent().GetId()
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: agent, Version: made.GetAgent().GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())
		set, err := c.Agents.SetAgentModel(ctx, &agentv1alpha1.SetAgentModelRequest{OrgId: org, AgentId: agent, Model: homeDrillModel})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetModel().GetEffective()).To(Equal(homeDrillModel))

		latestRun := func(task string) string {
			return fmt.Sprintf("SELECT id FROM runs WHERE task_id = '%s' ORDER BY id DESC LIMIT 1", task)
		}
		// awaitLatest waits until the task's newest run, after the first `after` runs, is in state want.
		awaitLatest := func(task string, after int, want string, timeout time.Duration) string {
			GinkgoHelper()
			var id string
			Eventually(func(g Gomega) {
				rows := psqlPrimary(ctx, g, kubectl, fmt.Sprintf("SELECT count(*) FROM runs WHERE task_id = '%s'", task))
				g.Expect(rows).To(HaveLen(1))
				n, err := strconv.Atoi(rows[0])
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(n).To(BeNumerically(">", after), "no new run of the task yet")
				// The outcome reason goes last: it is free text and can hold the field separator.
				rows = psqlPrimary(ctx, g, kubectl, fmt.Sprintf("SELECT id || '|' || state || '|' || coalesce(outcome_reason, '') FROM runs WHERE id = (%s)", latestRun(task)))
				g.Expect(rows).NotTo(BeEmpty())
				f := strings.SplitN(rows[0], "|", 3)
				g.Expect(f).To(HaveLen(3))
				id = f[0]
				if f[1] != want && (f[1] == "succeeded" || f[1] == "failed" || f[1] == "canceled") {
					StopTrying(fmt.Sprintf("run %s ended %s, not %s: %s", id, f[1], want, f[2])).Now()
				}
				g.Expect(f[1]).To(Equal(want), "run %s", id)
			}).WithContext(ctx).WithTimeout(timeout).WithPolling(10 * time.Second).Should(Succeed())
			return id
		}
		events := func(runID, like string) string {
			return one(fmt.Sprintf("SELECT count(*) FROM run_events WHERE run_id = '%s' AND payload::text LIKE '%%%s%%'", runID, like))
		}

		By("a task writes the marker into the agent's home")
		first, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent, Title: "Write the marker", Message: "home-drill-write: write the marker file into your working directory.",
		})
		Expect(err).NotTo(HaveOccurred())
		task := first.GetTask().GetId()
		awaitLatest(task, 0, "succeeded", 10*time.Minute)
		session := one(fmt.Sprintf("SELECT coalesce(harness_session_id, '') FROM tasks WHERE id = '%s'", task))
		Expect(session).NotTo(BeEmpty(), "the marker run made a harness session")

		By("the sandbox goes idle, stops with a stop checkpoint, and is archived (the actor and its durable directory are deleted)")
		Eventually(func(g Gomega) {
			g.Expect(psqlPrimary(ctx, g, kubectl, fmt.Sprintf("SELECT count(*) FROM agent_sandboxes WHERE agent_id = '%s' AND archived_at IS NOT NULL", agent))).
				To(Equal([]string{"1"}), "agent_sandboxes.archived_at is set")
		}).WithContext(ctx).WithTimeout(20 * time.Minute).WithPolling(15 * time.Second).Should(Succeed())
		Expect(one(fmt.Sprintf("SELECT count(*) FROM audit_events WHERE action = 'sandbox.archive' AND target_kind = 'agent' AND target_id = '%s'", agent))).
			NotTo(Equal("0"), "the reconciler recorded the archive")
		// Every complete checkpoint of the agent has a content digest: a fallback to an unverified one would hide a refusal.
		digests := sql(fmt.Sprintf("SELECT id || '|' || coalesce(content_digest, '') FROM home_checkpoints WHERE agent_id = '%s' AND status = 'complete' ORDER BY id", agent))
		Expect(digests).NotTo(BeEmpty(), "the archived agent has a complete checkpoint")
		saved := map[string]string{}
		for _, row := range digests {
			idDigest := strings.SplitN(row, "|", 2)
			Expect(idDigest).To(HaveLen(2))
			Expect(idDigest[1]).To(MatchRegexp(`^sha256:[0-9a-f]{64}$`), "checkpoint %s has a content digest", idDigest[0])
			saved[idDigest[0]] = idDigest[1]
		}
		setDigests := func(to func(id string) string) {
			GinkgoHelper()
			for id := range saved {
				sql(fmt.Sprintf("UPDATE home_checkpoints SET content_digest = '%s' WHERE id = '%s'", to(id), id))
			}
		}

		By("negative: with a wrong recorded digest, the restore is refused before the harness starts")
		setDigests(func(string) string { return wrongDigest })
		_, err = c.Tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: task, Text: "home-drill-read: is the marker file there?"})
		Expect(err).NotTo(HaveOccurred())
		refused := awaitLatest(task, 1, "failed", 15*time.Minute)
		Expect(one(fmt.Sprintf("SELECT coalesce(outcome_reason, '') FROM runs WHERE id = '%s'", refused))).
			To(ContainSubstring("does not match its recorded digest"), "the restore names the digest mismatch")
		Expect(events(refused, "home-drill-marker")).To(Equal("0"), "the harness did not run")

		By("the digests put back: the next message restores the home, verified, and loads the task's session")
		setDigests(func(id string) string { return saved[id] })
		_, err = c.Tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: task, Text: "home-drill-read: is the marker file there?"})
		Expect(err).NotTo(HaveOccurred())
		restored := awaitLatest(task, 2, "succeeded", 15*time.Minute)
		// The run printed "home-drill-marker-found" through its shell tool: the home came back from the bucket.
		Expect(events(restored, "home-drill-marker-found")).NotTo(Equal("0"), "the run read the marker")
		Expect(events(restored, "home-drill-marker-missing")).To(Equal("0"))
		Expect(one(fmt.Sprintf("SELECT count(*) FROM run_events WHERE run_id = '%s' AND kind = 'run.home_restored' AND payload->>'verified' = 'true'", restored))).
			To(Equal("1"), "the restore was checked against the recorded digest")
		Expect(one(fmt.Sprintf("SELECT coalesce(harness_session_id, '') FROM tasks WHERE id = '%s'", task))).To(Equal(session), "the run loaded the task's session, not a new one")
		Expect(one(fmt.Sprintf("SELECT count(*) FROM agent_sandboxes WHERE agent_id = '%s' AND archived_at IS NOT NULL", agent))).To(Equal("0"), "the agent is no longer archived")
	})
})
