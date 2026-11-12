// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// Obviously fake tokens (AGENTS.md).
const (
	giteaReadToken  = "koinon_test_not-a-real-gitea-read-token"
	giteaWriteToken = "koinon_test_not-a-real-gitea-write-token"
)

// newGiteaEnv is newGitHubEnv for a repo-helper agent on a fake Gitea (TLS, with a CA bundle on
// the connection): a bot with a read-scoped and a write-scoped token, a collaborator on
// acme/widgets only.
func newGiteaEnv(ctx context.Context) *ghEnv {
	g := &ghEnv{gt: fakegitea.NewTLS()}
	DeferCleanup(g.gt.Close)
	g.gt.Tokens = map[string]string{}
	g.gt.AddScopedToken(giteaReadToken, "koinon-bot", "read")
	g.gt.AddScopedToken(giteaWriteToken, "koinon-bot", "write")
	g.gt.AddRepo("widgets")
	g.gt.AddRepo("secrets")
	g.gt.Collaborate("koinon-bot", "widgets", "write")
	ring := secretstoretest.Keyring()
	g.env = newEnvWith(ctx, harness.OpenCode, func(e *env) string {
		connID := uuid.New()
		id := pgtype.UUID{Bytes: connID, Valid: true}
		envelope, err := connections.SealFields(ring, e.org, id, map[string][]byte{
			gitea.ReadTokenField: []byte(giteaReadToken), gitea.WriteTokenField: []byte(giteaWriteToken),
		})
		Expect(err).NotTo(HaveOccurred())
		cfg, _ := json.Marshal(gitea.NewConfig(codehost.Site{BaseURL: g.gt.URL(), CABundle: g.gt.CAPEM()},
			[]byte(giteaReadToken), []byte(giteaWriteToken), nil, time.Now()))
		_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: e.org, ID: id, Connector: "gitea", DisplayName: "lab",
			Config: cfg, SecretEnvelope: envelope, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		return `{"instructions":"help","model":"` + model + `",` +
			`"grants":[{"connector":"gitea","scopes":["repo.read","pull_request.create"],"preset":"standard"}],` +
			`"bindings":[{"connector":"gitea","connectionId":"` + connID.String() + `","targets":["acme/widgets"]}]}`
	})
	Expect(db.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, g.org).Scan(&g.slug)).To(Succeed())
	log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
	g.svc = approvals.New(db, approvals.Options{Keys: ring, Logger: log})
	tools, err := githubtools.New(db, githubtools.Options{Logger: log, Keys: ring})
	Expect(err).NotTo(HaveOccurred())
	g.tools = httptest.NewServer(tools.Handler())
	DeferCleanup(g.tools.Close)
	return g
}

// everythingInTheSandbox is all that was sent into the sandbox: every exec (command, arguments,
// environment, working directory, stdin), every file written, the prompts and arguments of the
// harness, and what OpenShell holds as the sandbox spec.
func (g *ghEnv) everythingInTheSandbox() string {
	var b strings.Builder
	for _, x := range g.gw.Execs() {
		raw, err := protojson.Marshal(x)
		Expect(err).NotTo(HaveOccurred())
		b.Write(raw)
		b.WriteByte('\n')
	}
	g.files.mu.Lock()
	for path, data := range g.files.data {
		b.WriteString(path + "\n")
		b.Write(data)
		b.WriteByte('\n')
	}
	for path, target := range g.files.links {
		b.WriteString(path + " -> " + target + "\n")
	}
	g.files.mu.Unlock()
	prompts, loads, _, args := g.harness.snapshot()
	b.WriteString(strings.Join(prompts, "\n") + strings.Join(loads, "\n"))
	for _, a := range args {
		b.WriteString(strings.Join(a, " ") + "\n")
	}
	raw, err := protojson.Marshal(g.gw.Sandbox(g.workspace, g.sandbox))
	Expect(err).NotTo(HaveOccurred())
	b.Write(raw)
	return b.String()
}

var _ = Describe("A repo-helper agent on Gitea", func() {
	It("fails the run with the reason, before the model and the sandbox, when the connection's credential was lost", func(ctx SpecContext) {
		g := newGiteaEnv(ctx)
		// What keys forget leaves behind (ADR 0085 decision 13): no envelope, and the mark.
		tag, err := db.Exec(ctx, `UPDATE connections SET secret_envelope = NULL, credential_lost_at = now() WHERE org_id = $1`, g.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(tag.RowsAffected()).To(BeEquivalentTo(1))
		// A priced model the gateway does not serve yet: the run would wait for it (minutes), but a
		// lost connection fails it before that wait.
		var providerID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'pending-upstream', 'openai', 'http://fakemodel.example.com/v1') RETURNING id`,
			g.org).Scan(&providerID)).To(Succeed())
		_, err = db.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, g.org, providerID, model)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, $2, jsonb_build_object('provider', 'openai', 'match', jsonb_build_object('glob', $3::text),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
			g.org, uniq("price"), model)
		Expect(err).NotTo(HaveOccurred())
		r := g.enqueue(ctx, g.task(ctx, "fix the typo"))
		g.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(eventOf(events(ctx, r), "run.waiting_for_model")).To(BeEmpty(), "it never waits for the model")
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring(
			`the stored credential of the gitea connection "lab" was lost; enter the secret of the connection "lab" again on the Connections page, then run the task again`))
		Expect(g.gw.Execs()).To(BeEmpty(), "nothing runs in a sandbox")
		_, _, _, args := g.harness.snapshot()
		Expect(args).To(BeEmpty(), "the harness never starts")
	})

	It("opens a pull request after one approval, and no token ever reaches the sandbox", func(ctx SpecContext) {
		g := newGiteaEnv(ctx)
		ids := g.script()
		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		g.pool("agentd-a")

		var id string
		Eventually(ids).WithTimeout(racescale.D(15 * time.Second)).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateParked))
		Expect(getRun(ctx, r).ParkReason.RunParkReason).To(Equal(store.RunParkReasonApproval))
		Expect(g.gt.Snapshot("widgets").Pulls).To(BeEmpty(), "nothing reaches Gitea before the approval")
		Expect(g.gt.Snapshot("widgets").Branches).To(HaveLen(1), "only main")

		By("one approval, and Koinon pushes the branch and opens the pull request with the write token")
		row, err := q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: g.org, ID: mustUUID(id)})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Connector).To(Equal("gitea"))
		decided, err := g.svc.Approve(ctx, g.org, row.ID, g.user, row.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(decided.Status).To(Equal(store.ApprovalStatusExecuted), decided.Error.String)

		snap := g.gt.Snapshot("widgets")
		Expect(snap.Pulls).To(HaveLen(1))
		branch := githubtools.BranchPrefix + githubtools.AgentNamespace(g.agent.DisplayName, g.agent.ID.String()) + "/fix-typo"
		Expect(snap.Pulls[0].Head).To(Equal(branch))
		Expect(snap.Pulls[0].Base).To(Equal("main"))
		Expect(snap.Branches).To(HaveKey(branch))
		Expect(snap.Branches["main"]).NotTo(Equal(snap.Branches[branch]), "the default branch is untouched")
		url := g.gt.URL() + "/acme/widgets/pulls/1"

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := g.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(ContainSubstring("approved and executed by Koinon: " + url))
		Expect(g.gt.Snapshot("secrets").Calls).To(BeEmpty(), "no other repository was touched")

		By("the instructions list the granted repository with the connection's clone URL")
		written, _ := g.files.get(instructionsFile)
		Expect(string(written)).To(ContainSubstring("`acme/widgets` on Gitea (connector `gitea`, host `127.0.0.1:"))
		Expect(string(written)).To(ContainSubstring("`git clone " + g.gt.URL() + "/acme/widgets`"))

		By("the sandbox received its Gitea host and CA, and no token")
		all := g.everythingInTheSandbox()
		Expect(all).To(ContainSubstring("koinon-git-credential --host 127.0.0.1:"), "the credential helper is configured for the connection's host")
		Expect(all).To(ContainSubstring("BEGIN CERTIFICATE"), "the connection's CA bundle")
		for _, tok := range []string{giteaReadToken, giteaWriteToken, "not-a-real-gitea"} {
			Expect(all).NotTo(ContainSubstring(tok))
		}
	})
})
