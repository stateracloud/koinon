// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// fakeChat stands in for the gateway: it answers every check with reply, or fails with err.
type fakeChat struct {
	reply string
	err   error
	calls []string
}

func (f *fakeChat) Complete(_ context.Context, _ pgtype.UUID, model string, _ modelformat.Format, _ string) (string, error) {
	f.calls = append(f.calls, model)
	return f.reply, f.err
}

// serveModel adds a priced model on a new provider and returns the provider's ID.
func serveModel(ctx context.Context, e *env, name string, priced bool) string {
	GinkgoHelper()
	var providerID pgtype.UUID
	Expect(pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openai', 'http://fakemodel.example.com/v1') RETURNING id`,
		e.org, "prov-"+strings.NewReplacer("/", "-").Replace(name)).Scan(&providerID)).To(Succeed())
	_, err := pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, e.org, providerID, name)
	Expect(err).NotTo(HaveOccurred())
	// The org's first model is its default, as catalog.CreateModel does (old issue 256).
	_, err = pool.Exec(ctx, `UPDATE orgs SET default_model = $2 WHERE id = $1 AND default_model IS NULL`, e.org, name)
	Expect(err).NotTo(HaveOccurred())
	if priced {
		_, err = pool.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, $2, jsonb_build_object('provider', 'openai', 'match', jsonb_build_object('glob', $3::text),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
			e.org, "price-"+strings.NewReplacer("/", "-").Replace(name), name)
		Expect(err).NotTo(HaveOccurred())
	}
	return providerID.String()
}

// agentWith creates an active agent whose version has the given config.
func agentWith(ctx context.Context, e *env, config string) string {
	GinkgoHelper()
	q := store.New(pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: e.org, DisplayName: "a", Flavor: store.AgentFlavorOpencode, CreatedBy: e.user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: e.org, AgentID: a.ID, Config: []byte(config), CreatedBy: e.user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: e.org, ID: a.ID, VersionID: v.ID, ActivatedBy: e.user})
	Expect(err).NotTo(HaveOccurred())
	return a.ID.String()
}

var _ = Describe("agents in the admin API (rc.6)", func() {
	var e *env
	BeforeEach(func(ctx SpecContext) { e = newEnv(ctx) })

	Describe("the agent's model", func() {
		It("sets and clears the model, shows where the effective one comes from, and audits it", func(ctx SpecContext) {
			serveModel(ctx, e, "vendor/a-model", true)
			serveModel(ctx, e, "vendor/unpriced", false)
			agent := agentWith(ctx, e, `{"instructions":"x","model":"default"}`)

			st, raw := e.call(ctx, e.token, http.MethodGet, "/agents/"+agent+"/model", nil)
			Expect(st).To(Equal(http.StatusOK), string(raw))
			got := decode[map[string]any](raw)
			Expect(got).To(HaveKeyWithValue("setting", "default"))
			Expect(got).To(HaveKeyWithValue("source", "org_default"))
			Expect(got).To(HaveKeyWithValue("effective", "vendor/a-model"))
			Expect(got["available"]).To(HaveLen(1), "an unpriced model is not offered")

			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/model", map[string]any{"model": "vendor/a-model"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			got = decode[map[string]any](raw)
			Expect(got).To(HaveKeyWithValue("setting", "vendor/a-model"))
			Expect(got).To(HaveKeyWithValue("source", "agent"))

			for _, bad := range []string{"vendor/ghost", "vendor/unpriced"} {
				st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/model", map[string]any{"model": bad})
				Expect(st).To(Equal(http.StatusBadRequest), string(raw))
			}

			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/model", map[string]any{"model": "default"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("setting", "default"))

			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.model.set' AND target_id = $2
				AND actor_kind = 'token' AND summary->'before' IS NOT NULL AND summary->'after' IS NOT NULL`, e.org, agent).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(2))

			st, _ = e.call(ctx, e.token, http.MethodGet, "/agents/019a0000-0000-7000-8000-00000000ffff/model", nil)
			Expect(st).To(Equal(http.StatusNotFound))
			st, _ = e.call(ctx, e.token, http.MethodPut, "/agents/019a0000-0000-7000-8000-00000000ffff/model", map[string]any{"model": "default"})
			Expect(st).To(Equal(http.StatusNotFound))
		})
	})

	Describe("the agent's thinking (rc.8)", func() {
		It("is on by default, turns off and clears, says when the model cannot think, and audits it", func(ctx SpecContext) {
			serveModel(ctx, e, "vendor/a-model", true)
			agent := agentWith(ctx, e, `{"instructions":"x","model":"default"}`)

			st, raw := e.call(ctx, e.token, http.MethodGet, "/agents/"+agent+"/model", nil)
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)["thinking"]).To(Equal(map[string]any{
				"setting": "default", "effective": "on", "supported": false, "mode": "none",
				"alwaysThinks": false, "offOverridden": false,
			}), "on by default; a model the catalog does not list ignores it")

			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/thinking", map[string]any{"thinking": "off"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)["thinking"]).To(And(
				HaveKeyWithValue("setting", "off"), HaveKeyWithValue("effective", "off")))

			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/thinking", map[string]any{"thinking": "high"})
			Expect(st).To(Equal(http.StatusBadRequest), string(raw))

			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/thinking", map[string]any{"thinking": "default"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)["thinking"]).To(HaveKeyWithValue("setting", "default"))

			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.thinking.set' AND target_id = $2
				AND actor_kind = 'token' AND summary->'before' IS NOT NULL AND summary->'after' IS NOT NULL`, e.org, agent).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(2))

			st, _ = e.call(ctx, e.token, http.MethodPut, "/agents/019a0000-0000-7000-8000-00000000ffff/thinking", map[string]any{"thinking": "off"})
			Expect(st).To(Equal(http.StatusNotFound))
		})

		// old issue 316: off exists only where the model can really stop thinking.
		It("refuses off for a model that always thinks, and keeps a stored off that a later model change overrides", func(ctx SpecContext) {
			serveModel(ctx, e, "vendor/a-model", true)
			serveModel(ctx, e, "gpt-6-astra", true)
			agent := agentWith(ctx, e, `{"instructions":"x","model":"gpt-6-astra"}`)

			st, raw := e.call(ctx, e.token, http.MethodGet, "/agents/"+agent+"/model", nil)
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)["thinking"]).To(Equal(map[string]any{
				"setting": "default", "effective": "on", "supported": true, "mode": "default",
				"alwaysThinks": true, "offOverridden": false,
			}))
			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/thinking", map[string]any{"thinking": "off"})
			Expect(st).To(Equal(http.StatusBadRequest), string(raw))
			Expect(string(raw)).To(ContainSubstring("this model always thinks"))

			By("off is stored while the model cannot think, and a change to a model that always thinks is not refused")
			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/model", map[string]any{"model": "vendor/a-model"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/thinking", map[string]any{"thinking": "off"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			st, raw = e.call(ctx, e.token, http.MethodPut, "/agents/"+agent+"/model", map[string]any{"model": "gpt-6-astra"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)["thinking"]).To(Equal(map[string]any{
				"setting": "off", "effective": "off", "supported": true, "mode": "default",
				"alwaysThinks": true, "offOverridden": true, "notice": "this model always thinks; your off setting doesn't apply",
			}), "the stored off is kept and runs think")
		})
	})

	Describe("running a schedule trigger now", func() {
		const cfg = `{"instructions":"x","triggers":[
			{"id":"daily-check","type":"schedule","filter":{"cron":"0 9 * * *","timezone":"UTC"},"instruction":"Check the costs."},
			{"id":"chat","type":"direct_message","instruction":"Answer."}]}`

		It("creates a manual task with the trigger's instruction, and audits it", func(ctx SpecContext) {
			agent := agentWith(ctx, e, cfg)
			st, raw := e.call(ctx, e.token, http.MethodPost, "/agents/"+agent+"/triggers/daily-check/run", nil)
			Expect(st).To(Equal(http.StatusAccepted), string(raw))
			task := decode[map[string]any](raw)["taskId"].(string)
			var kind, fire, text string
			Expect(pool.QueryRow(ctx, `SELECT t.created_by_kind::text, m.payload->>'fire', m.payload->>'text'
				FROM tasks t JOIN task_events m ON m.org_id = t.org_id AND m.task_id = t.id AND m.kind = 'message' WHERE t.id = $1`, task).Scan(&kind, &fire, &text)).To(Succeed())
			Expect([]string{kind, fire, text}).To(Equal([]string{"trigger", "manual", "Check the costs."}))
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.trigger.run_now' AND actor_kind = 'token'`, e.org).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))
		})

		It("answers 404 for what is not a schedule trigger of the active version, 409 for an agent that cannot run", func(ctx SpecContext) {
			agent := agentWith(ctx, e, cfg)
			for _, id := range []string{"chat", "nothing"} {
				st, _ := e.call(ctx, e.token, http.MethodPost, "/agents/"+agent+"/triggers/"+id+"/run", nil)
				Expect(st).To(Equal(http.StatusNotFound), id)
			}
			_, err := pool.Exec(ctx, `UPDATE agents SET unhealthy_at = now(), unhealthy_reason = 'stuck' WHERE id = $1`, agent)
			Expect(err).NotTo(HaveOccurred())
			st, raw := e.call(ctx, e.token, http.MethodPost, "/agents/"+agent+"/triggers/daily-check/run", nil)
			Expect(st).To(Equal(http.StatusConflict), string(raw))
			st, _ = e.call(ctx, e.token, http.MethodPost, "/agents/019a0000-0000-7000-8000-00000000ffff/triggers/daily-check/run", nil)
			Expect(st).To(Equal(http.StatusNotFound))
		})
	})

	Describe("the provider echo check", func() {
		It("runs on demand, stores the result on the provider, and audits it", func(ctx SpecContext) {
			provider := serveModel(ctx, e, "vendor/a-model", true)
			e.chat.reply = strings.Replace(modelcheck.Line, "Maria Schneider", "[NAME]", 1)

			st, raw := e.call(ctx, e.token, http.MethodPost, "/providers/"+provider+"/echo-check", map[string]any{"model": "vendor/a-model"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			got := decode[map[string]any](raw)
			Expect(got).To(HaveKeyWithValue("unchanged", false))
			Expect(got).To(HaveKeyWithValue("sent", modelcheck.Line))
			Expect(got["diff"]).NotTo(BeEmpty())

			st, raw = e.call(ctx, e.token, http.MethodGet, "/providers/"+provider, nil)
			Expect(st).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)["echoCheck"]).To(HaveKeyWithValue("unchanged", false))

			e.chat.reply = modelcheck.Line
			st, raw = e.call(ctx, e.token, http.MethodPost, "/providers/"+provider+"/echo-check", map[string]any{"model": "vendor/a-model"})
			Expect(st).To(Equal(http.StatusOK), string(raw))
			st, raw = e.call(ctx, e.token, http.MethodGet, "/providers", nil)
			Expect(st).To(Equal(http.StatusOK))
			list := decode[struct{ Providers []map[string]any }](raw).Providers
			Expect(list).To(HaveLen(1))
			Expect(list[0]["echoCheck"]).To(HaveKeyWithValue("unchanged", true))
			Expect(e.chat.calls).To(Equal([]string{"vendor/a-model", "vendor/a-model"}), "one request per call, nothing automatic")
		})

		It("refuses a model the provider does not serve and reports a gateway failure as 503", func(ctx SpecContext) {
			provider := serveModel(ctx, e, "vendor/a-model", true)
			st, _ := e.call(ctx, e.token, http.MethodPost, "/providers/"+provider+"/echo-check", map[string]any{"model": "vendor/other"})
			Expect(st).To(Equal(http.StatusBadRequest))
			e.chat.err = errors.New("the gateway answered 404 Not Found")
			st, raw := e.call(ctx, e.token, http.MethodPost, "/providers/"+provider+"/echo-check", map[string]any{"model": "vendor/a-model"})
			Expect(st).To(Equal(http.StatusServiceUnavailable), string(raw))
			Expect(fmt.Sprint(e.chat.calls)).To(Equal("[vendor/a-model]"))
			st, _ = e.call(ctx, e.token, http.MethodPost, "/providers/019a0000-0000-7000-8000-00000000ffff/echo-check", map[string]any{"model": "vendor/a-model"})
			Expect(st).To(Equal(http.StatusNotFound))
		})
	})
})
