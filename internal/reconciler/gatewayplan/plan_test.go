// SPDX-License-Identifier: Apache-2.0

package gatewayplan_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

const keyDir = "/secrets/provider-keys"

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func uuid(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{15: n}, Valid: true} }

var (
	acme   = gatewayplan.Org{ID: uuid(1), Slug: "acme"}
	globex = gatewayplan.Org{ID: uuid(2), Slug: "globex"}
)

// agent returns an active agent of acme with a key; its ref is acme/<name>.
func agent(org gatewayplan.Org, n byte, name string) gatewayplan.Agent {
	ref := gateway.AgentRef{Org: org.Slug, Name: name}
	return gatewayplan.Agent{
		Org: org, ID: uuid(n), Ref: ref, Active: true,
		Key:   gateway.Key{ID: "key-" + name, Agent: ref, Hash: gateway.HashKey("koinon_test_not-a-real-token-" + name)},
		Limit: &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("5"), Window: budget.DayWindow, Generation: dayGen(t0, "1")},
	}
}

func dayGen(now time.Time, suffix string) string {
	return strconv.FormatInt(budget.DayStart(now).Unix(), 10) + "-" + suffix
}

func provider(org gatewayplan.Org, id byte, name, file string) gatewayplan.Provider {
	gp := gateway.Provider{
		Org: org.Slug, Name: name, Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
		Models: []gateway.Model{{Name: "open/model", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "2"}}},
	}
	if file != "" {
		gp.APIKeyFile = path.Join(keyDir, file)
	}
	return gatewayplan.Provider{ID: "prov-" + name, OrgID: org.ID, ProviderID: uuid(id), File: file, Gateway: gp}
}

func newPlanner(settle time.Duration) *gatewayplan.Planner {
	return gatewayplan.New(gatewayplan.Config{
		KeyDir: keyDir, KeySettle: settle, MCPAskURL: "http://koinon-api:8080/mcp",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolServers: map[string]string{
			gateway.KoinonTarget: "http://koinon-api:8081/koinon", "github": "http://koinon-api:8081/github",
		},
	})
}

// fakeLookups answers the reads a settle makes, from memory.
type fakeLookups struct {
	servedFile  map[pgtype.UUID]string
	systemErr   error
	limitErr    error
	issued      int
	systemCalls []string
}

func (f *fakeLookups) SystemKey(_ context.Context, secret string, _ gatewayplan.Org, ref gateway.AgentRef) (gateway.Key, error) {
	f.systemCalls = append(f.systemCalls, ref.String())
	if f.systemErr != nil {
		return gateway.Key{}, f.systemErr
	}
	return gateway.Key{ID: "sys-" + ref.Org + "-" + ref.Name, Agent: ref, Hash: gateway.HashKey(secret)}, nil
}

func (f *fakeLookups) IssueKey(_ context.Context, ref gateway.AgentRef) (gateway.IssuedKey, error) {
	f.issued++
	m := "koinon_test_not-a-real-token-probe-" + strconv.Itoa(f.issued)
	return gateway.IssuedKey{Key: gateway.Key{ID: "probe-" + strconv.Itoa(f.issued), Agent: ref, Hash: gateway.HashKey(m)}, Material: m}, nil
}

func (f *fakeLookups) SystemLimit(context.Context, gatewayplan.Org, time.Time) (*gateway.Limit, error) {
	if f.limitErr != nil {
		return nil, f.limitErr
	}
	return &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: budget.DayWindow, Generation: dayGen(t0, "9")}, nil
}

func (f *fakeLookups) ServedKeyFile(_ context.Context, _, provider pgtype.UUID) (string, bool, error) {
	file, ok := f.servedFile[provider]
	return file, ok, nil
}

func settle(d *gatewayplan.Draft, written []string, present map[string]bool, lk gatewayplan.Lookups) *gatewayplan.Plan {
	pl, err := d.Settle(context.Background(), written, present, lk)
	Expect(err).NotTo(HaveOccurred())
	return pl
}

func keyIDs(s gateway.State) []string {
	var out []string
	for _, k := range s.Keys {
		out = append(out, k.ID)
	}
	return out
}

var _ = Describe("Planner", func() {
	var lk *fakeLookups
	BeforeEach(func() { lk = &fakeLookups{servedFile: map[pgtype.UUID]string{}} })

	Describe("an omitted key means revoked", func() {
		It("carries the keys of this pass only", func() {
			archived := agent(acme, 3, "archived")
			archived.Active, archived.Key, archived.Limit = false, gateway.Key{}, nil
			p := newPlanner(0)
			in := gatewayplan.Input{Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{agent(acme, 1, "a1"), archived, agent(acme, 2, "a2")}}
			pl := settle(p.Begin(in), nil, nil, lk)
			Expect(keyIDs(pl.State)).To(Equal([]string{"key-a1", "key-a2"}))

			// The next pass without a2 no longer names its key or its limit.
			in.Agents = in.Agents[:1]
			pl = settle(p.Begin(in), nil, nil, lk)
			Expect(keyIDs(pl.State)).To(Equal([]string{"key-a1"}))
			Expect(pl.State.Limits).NotTo(HaveKey(gateway.AgentRef{Org: "acme", Name: "a2"}))
		})
	})

	Describe("a limit that cannot be computed fails closed", func() {
		failing := func(a gatewayplan.Agent) gatewayplan.Agent {
			a.Limit, a.LimitErr = nil, errors.New("ledger unreachable")
			return a
		}
		run := func(p *gatewayplan.Planner, now time.Time, enforces bool, a gatewayplan.Agent) (*gatewayplan.Draft, *gateway.Limit) {
			d := p.Begin(gatewayplan.Input{Now: now, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{a}, LedgerEnforcesBudgets: enforces})
			pl := settle(d, nil, nil, lk)
			l, ok := pl.State.Limits[a.Key.Agent]
			Expect(ok).To(BeTrue())
			return d, &l
		}

		It("keeps the last pushed limit of the same UTC day", func() {
			p := newPlanner(0)
			a := agent(acme, 1, "a1")
			run(p, t0, false, a)
			d, l := run(p, t0.Add(time.Hour), false, failing(a))
			Expect(l.Amount).To(Equal(money.MustParse("5")))
			Expect(d.LimitFailures).To(HaveLen(1))
			Expect(d.LimitFailures[0].Agent).To(Equal(0))
		})

		It("goes to zero on a later day, so a failing computation does not regrant it", func() {
			p := newPlanner(0)
			a := agent(acme, 1, "a1")
			run(p, t0, false, a)
			_, l := run(p, t0.Add(24*time.Hour), false, failing(a))
			Expect(l.Amount).To(Equal(money.MustParse("0")))
			Expect(l.Generation).To(Equal("failed"))
		})

		It("goes to zero for an agent that never had a limit", func() {
			_, l := run(newPlanner(0), t0, false, failing(agent(acme, 1, "a1")))
			Expect(l.Amount).To(Equal(money.MustParse("0")))
		})

		It("goes to zero when the ledger enforces budgets (Kubernetes mode), whatever it pushed before", func() {
			p := newPlanner(0)
			a := agent(acme, 1, "a1")
			run(p, t0, true, a)
			_, l := run(p, t0, true, failing(a))
			Expect(l.Amount).To(Equal(money.MustParse("0")))
		})

		It("stops nothing for it: it is not known to be spent", func() {
			d, _ := run(newPlanner(0), t0, false, failing(agent(acme, 1, "a1")))
			Expect(d.Stops).To(BeEmpty())
		})
	})

	Describe("a budget stop comes before the revoke", func() {
		It("stops the agents whose limit has nothing left, before any Apply", func() {
			spent := agent(acme, 1, "spent")
			spent.Limit = &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: budget.DayWindow, Generation: dayGen(t0, "2")}
			unlimited := agent(acme, 2, "unlimited")
			unlimited.Limit = nil
			d := newPlanner(0).Begin(gatewayplan.Input{Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{spent, agent(acme, 3, "fine"), unlimited}})
			Expect(d.Stops).To(Equal([]int{0}))
		})
	})

	Describe("a key file is written before the config that names it", func() {
		in := func(file string) gatewayplan.Input {
			return gatewayplan.Input{
				Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{agent(acme, 1, "a1")},
				Providers:     []gatewayplan.Provider{provider(acme, 10, "openrouter", file)},
				ProviderFiles: map[string][]byte{file: []byte("koinon_test_not-a-real-token")},
			}
		}

		It("returns the files to write, and leaves a provider out while its new file settles", func() {
			p := newPlanner(time.Minute)
			d := p.Begin(in("prov-f1"))
			Expect(d.Files).To(HaveKey("prov-f1"))
			pl := settle(d, []string{"prov-f1"}, map[string]bool{"prov-f1": true}, lk)
			Expect(pl.State.Providers).To(BeEmpty())
			Expect(pl.Keep).To(HaveKey("prov-f1"))
		})

		It("serves the provider once the file has had the settle time", func() {
			p := newPlanner(time.Minute)
			settle(p.Begin(in("prov-f1")), []string{"prov-f1"}, nil, lk)
			later := in("prov-f1")
			later.Now = t0.Add(2 * time.Minute)
			pl := settle(p.Begin(later), nil, nil, lk)
			Expect(pl.State.Providers).To(HaveLen(1))
			Expect(pl.State.Providers[0].APIKeyFile).To(Equal(path.Join(keyDir, "prov-f1")))
		})

		It("keeps serving with the previous file while a rotated key settles, and keeps that file", func() {
			p := newPlanner(time.Minute)
			first := in("prov-f1")
			first.Now = t0.Add(-time.Hour)
			pl := settle(p.Begin(first), nil, nil, lk)
			pl.Applied(gatewayplan.ApplyResult{})

			rot := in("prov-f2")
			rot.Now = t0
			pl = settle(p.Begin(rot), []string{"prov-f2"}, nil, lk)
			Expect(pl.State.Providers).To(HaveLen(1))
			Expect(pl.State.Providers[0].APIKeyFile).To(Equal(path.Join(keyDir, "prov-f1")))
			Expect(pl.Keep).To(And(HaveKey("prov-f1"), HaveKey("prov-f2")))
		})

		It("falls back to the recorded file after a restart when it is still in the Secret", func() {
			lk.servedFile[uuid(10)] = "prov-f0"
			pl := settle(newPlanner(time.Minute).Begin(in("prov-f1")), []string{"prov-f1"}, map[string]bool{"prov-f0": true, "prov-f1": true}, lk)
			Expect(pl.State.Providers).To(HaveLen(1))
			Expect(pl.State.Providers[0].APIKeyFile).To(Equal(path.Join(keyDir, "prov-f0")))

			// A recorded file the Secret no longer holds is no fallback.
			pl = settle(newPlanner(time.Minute).Begin(in("prov-f1")), []string{"prov-f1"}, map[string]bool{"prov-f1": true}, lk)
			Expect(pl.State.Providers).To(BeEmpty())
		})

		It("does not record a file for an org whose model objects the gateway rejected", func() {
			p := newPlanner(time.Minute)
			old := in("prov-f1")
			old.Now = t0.Add(-time.Hour)
			pl := settle(p.Begin(old), nil, nil, lk)
			out := pl.Applied(gatewayplan.ApplyResult{Rejections: []gateway.Rejection{{Org: "acme", Kind: "AgentgatewayModel", Name: "m", Reason: "bad"}}})
			Expect(out.Served).To(HaveLen(1))
			Expect(out.Served[0].PruneOnly).To(BeTrue())

			// Nothing was applied for acme, so a rotated file settling has no previous file to serve with.
			pl = settle(p.Begin(in("prov-f2")), []string{"prov-f2"}, nil, lk)
			Expect(pl.State.Providers).To(BeEmpty())
		})

		It("records the models served with their key files", func() {
			pl := settle(newPlanner(0).Begin(withModels(in("prov-f1"))), nil, nil, lk)
			out := pl.Applied(gatewayplan.ApplyResult{})
			Expect(out.Served).To(HaveLen(1))
			Expect(out.Served[0].PruneOnly).To(BeFalse())
			Expect(out.Served[0].IDs).To(Equal([]pgtype.UUID{uuid(50)}))
			Expect(out.Served[0].Files).To(Equal([]string{"prov-f1"}))
		})
	})
})

// withModels gives the input the ID of the provider's model, as the loader does.
func withModels(in gatewayplan.Input) gatewayplan.Input {
	in.ModelIDs = map[string]pgtype.UUID{gateway.ModelRef("acme", "open/model"): uuid(50)}
	return in
}

var _ = Describe("Key liveness", func() {
	var lk *fakeLookups
	BeforeEach(func() { lk = &fakeLookups{servedFile: map[pgtype.UUID]string{}} })

	in := func(agents ...gatewayplan.Agent) gatewayplan.Input {
		return gatewayplan.Input{Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: agents}
	}

	It("holds everything back after a restart until something is applied", func() {
		p := newPlanner(0)
		pl := settle(p.Begin(in(agent(acme, 1, "a1"), agent(acme, 2, "a2"))), nil, nil, lk)
		// Nothing was issued by this process, but nothing was applied either: no key counts as live.
		Expect(pl.Early).To(BeEmpty())
		Expect(pl.Late).To(Equal([]int{0, 1}))

		out := pl.Applied(gatewayplan.ApplyResult{Failed: true})
		Expect(out.Held).To(Equal([]int{0, 1}))
		Expect(out.Run).To(BeEmpty())
		Expect(out.Served).To(BeNil())

		out = pl.Applied(gatewayplan.ApplyResult{})
		Expect(out.Run).To(Equal([]int{0, 1}))

		// Once applied, keys the gateway serves start without waiting.
		pl = settle(p.Begin(in(agent(acme, 1, "a1"), agent(acme, 2, "a2"))), nil, nil, lk)
		Expect(pl.Early).To(Equal([]int{0, 1}))
		Expect(pl.Late).To(BeEmpty())
	})

	It("makes only a new key wait for the Apply", func() {
		p := newPlanner(0)
		settle(p.Begin(in(agent(acme, 1, "a1"))), nil, nil, lk).Applied(gatewayplan.ApplyResult{})

		p.KeyIssued("key-a2")
		pl := settle(p.Begin(in(agent(acme, 1, "a1"), agent(acme, 2, "a2"))), nil, nil, lk)
		Expect(pl.Early).To(Equal([]int{0}))
		Expect(pl.Late).To(Equal([]int{1}))

		// A failing Apply holds the new key back; the next successful one releases it.
		Expect(pl.Applied(gatewayplan.ApplyResult{Failed: true}).Held).To(Equal([]int{1}))
		Expect(pl.Applied(gatewayplan.ApplyResult{}).Run).To(Equal([]int{1}))
		Expect(p.KeyNotLive("key-a2")).To(BeFalse())
	})

	It("fails one rejected key alone", func() {
		p := newPlanner(0)
		settle(p.Begin(in(agent(acme, 1, "a1"))), nil, nil, lk).Applied(gatewayplan.ApplyResult{})
		p.KeyIssued("key-a2")
		p.KeyIssued("key-a3")
		pl := settle(p.Begin(in(agent(acme, 2, "a2"), agent(acme, 3, "a3"))), nil, nil, lk)

		out := pl.Applied(gatewayplan.ApplyResult{Rejections: []gateway.Rejection{{Org: "acme", Kind: "Key", Name: "key-a2", Reason: "bad hash"}}})
		Expect(out.Held).To(Equal([]int{0}))
		Expect(out.Run).To(Equal([]int{1}))
		Expect(p.KeyNotLive("key-a2")).To(BeTrue())
		Expect(p.KeyNotLive("key-a3")).To(BeFalse())
	})

	It("does not treat an agent without a key, or an archived one, as waiting", func() {
		p := newPlanner(0)
		archived := agent(acme, 2, "archived")
		archived.Active = false
		keyless := agent(acme, 3, "keyless")
		keyless.Key = gateway.Key{}
		pl := settle(p.Begin(in(archived, keyless)), nil, nil, lk)
		Expect(pl.Early).To(Equal([]int{0, 1}))
	})
})

var _ = Describe("Model check", func() {
	var lk *fakeLookups
	BeforeEach(func() { lk = &fakeLookups{servedFile: map[pgtype.UUID]string{}} })

	in := func() gatewayplan.Input {
		return gatewayplan.Input{
			Now: t0, Orgs: []gatewayplan.Org{acme, globex},
			Providers: []gatewayplan.Provider{provider(acme, 10, "openrouter", "")},
		}
	}
	ref := gateway.AgentRef{Org: "acme", Name: gateway.ModelCheckAgentName}

	It("gives an org that serves a priced model a key restricted to those models, and no other org", func() {
		pl := settle(newPlanner(0).Begin(in()), nil, nil, lk)
		Expect(lk.systemCalls).To(Equal([]string{ref.String()}))
		Expect(keyIDs(pl.State)).To(Equal([]string{"sys-acme-model-check"}))
		Expect(pl.State.Models[ref]).To(Equal([]string{"open/model"}))
		Expect(pl.State.Limits[ref].Amount).To(Equal(money.MustParse("1")))
		Expect(pl.Problems).To(BeEmpty())
	})

	It("fails the key closed when its budget cannot be computed, and reports it", func() {
		lk.limitErr = errors.New("ledger unreachable")
		pl := settle(newPlanner(0).Begin(in()), nil, nil, lk)
		Expect(pl.State.Limits[ref].Amount).To(Equal(money.MustParse("0")))
		Expect(pl.Problems).To(HaveLen(1))
		Expect(pl.Problems[0].Kind).To(Equal(gatewayplan.ProblemSpendLimit))
	})

	It("leaves the org without a key, and reports it, when the key cannot be had", func() {
		lk.systemErr = errors.New("secret store down")
		pl := settle(newPlanner(0).Begin(in()), nil, nil, lk)
		Expect(pl.State.Keys).To(BeEmpty())
		Expect(pl.Problems).To(HaveLen(1))
		Expect(pl.Problems[0].Kind).To(Equal(gatewayplan.ProblemProvider))
	})
})

// mcpInput is an org with one MCP server, one agent allowed an auto and an ask tool, and an unhealthy one.
func mcpInput() gatewayplan.Input {
	srv := gatewayplan.MCPServer{
		Org: acme, ID: uuid(20), Name: "wiki", URL: "https://wiki.example.com/mcp",
		CredentialHeader: "authorization", CredentialPrefix: "Bearer ", File: "mcp-wiki-f1",
		Tools: []gatewayplan.MCPTool{
			{Name: "search", Present: true, Tier: gatewayplan.TierAuto},
			{Name: "edit", Present: true, Tier: gatewayplan.TierAsk},
			{Name: "drop", Present: true, Tier: gatewayplan.TierNever},
			{Name: "gone", Present: false, Tier: gatewayplan.TierAuto},
			{Name: "bad.name", Present: true, Tier: gatewayplan.TierAuto},
		},
	}
	sick := srv
	sick.ID, sick.Name, sick.File, sick.Unhealthy = uuid(21), "sick", "", true
	return gatewayplan.Input{
		Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{agent(acme, 1, "a1")},
		MCP: []gatewayplan.MCPServer{srv, sick}, MCPFiles: map[string][]byte{"mcp-wiki-f1": []byte("koinon_test_not-a-real-token")},
	}
}

var _ = Describe("MCP servers", func() {
	var lk *fakeLookups
	BeforeEach(func() { lk = &fakeLookups{servedFile: map[pgtype.UUID]string{}} })

	allow := map[string]map[pgtype.UUID][]gatewayplan.Allowed{
		"acme": {uuid(1): {{Server: uuid(20)}, {Server: uuid(21)}}},
	}

	It("grants tools by tier, adds the ask target, the executor key and a probe key per server", func() {
		p := newPlanner(0)
		in := mcpInput()
		in.Allowances = allow
		d := p.Begin(in)
		pl := settle(d, nil, nil, lk)

		a1 := gateway.AgentRef{Org: "acme", Name: "a1"}
		Expect(pl.State.Tools[a1]).To(ContainElements(
			gateway.ToolGrant{Target: "acme-wiki", Tools: []string{"search"}},
			gateway.ToolGrant{Target: "acme-wiki-ask", Tools: []string{"edit", mcpservers.AskStatusTool}},
		))
		for _, g := range pl.State.Tools[a1] {
			Expect(g.Target).NotTo(ContainSubstring("sick"), "an unhealthy server gives no tools")
		}
		var names []string
		for _, t := range pl.State.MCP {
			names = append(names, t.Name)
		}
		Expect(names).To(ContainElements("acme-wiki", "acme-wiki-ask", "acme-sick"))
		exec := gateway.AgentRef{Org: "acme", Name: gateway.MCPExecAgentName}
		// The executor opens every tool a person can approve a call of, healthy server or not (as
		// before old issue 455): the ask tier (ADR 0042) and the auto tier, whose external calls the leak
		// guard sends to a person (ADR 0091). Never a never-tier, absent or unnamable tool.
		Expect(pl.State.Tools[exec]).To(Equal([]gateway.ToolGrant{
			{Target: "acme-wiki", Tools: []string{"search", "edit"}},
			{Target: "acme-sick", Tools: []string{"search", "edit"}},
		}))
		Expect(pl.State.Tools).To(HaveKey(gateway.AgentRef{Org: "acme", Name: gateway.MCPProbeAgentPrefix + "wiki"}))
		Expect(lk.issued).To(Equal(2))
		Expect(p.Probes()).To(BeEmpty(), "probes exist only once the state is applied")

		pl.Applied(gatewayplan.ApplyResult{})
		Expect(p.Probes()).To(HaveLen(2))
		Expect(p.Probes()[0].Material).NotTo(BeEmpty())
	})

	It("keeps a file written in a pass that fails in Settle settling in the next pass, which writes nothing", func() {
		p := newPlanner(time.Minute)
		in := mcpInput()
		in.Allowances = allow
		lk.systemErr = errors.New("no key")
		_, err := p.Begin(in).Settle(context.Background(), []string{"mcp-wiki-f1"}, nil, lk)
		Expect(err).To(HaveOccurred())

		// The next pass writes nothing (the file is unchanged), yet the file has not had KeySettle.
		lk.systemErr = nil
		pl := settle(p.Begin(in), nil, nil, lk)
		for _, t := range pl.State.MCP {
			Expect(t.Name).NotTo(Equal("acme-wiki"))
		}
		Expect(pl.Keep).To(HaveKey("mcp-wiki-f1"))
	})

	It("wakes the reconciler once, at the end of the settle of a pass that wrote key files (#249)", func() {
		woken := make(chan struct{}, 4)
		p := gatewayplan.New(gatewayplan.Config{
			KeyDir: keyDir, KeySettle: 100 * time.Millisecond, MCPAskURL: "http://koinon-api:8080/mcp",
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			ToolServers: map[string]string{gateway.KoinonTarget: "http://koinon-api:8081/koinon"},
			Wake:        func() { woken <- struct{}{} },
		})
		in := mcpInput()
		in.Allowances = allow
		start := time.Now()
		_, err := p.Begin(in).Settle(context.Background(), []string{"mcp-wiki-f1", "mcp-wiki-f2"}, nil, lk)
		Expect(err).NotTo(HaveOccurred())
		Consistently(woken, 50*time.Millisecond).ShouldNot(Receive(), "not before the settle ends")
		Eventually(woken, time.Second).Should(Receive())
		Expect(time.Since(start)).To(BeNumerically(">=", 100*time.Millisecond))
		Consistently(woken, 200*time.Millisecond).ShouldNot(Receive(), "one wake per pass, not one per file")

		// A pass that writes nothing schedules nothing.
		_, err = p.Begin(in).Settle(context.Background(), nil, nil, lk)
		Expect(err).NotTo(HaveOccurred())
		Consistently(woken, 200*time.Millisecond).ShouldNot(Receive())
	})

	It("does not wake after Close: a pending settle-end timer is stopped (#249)", func() {
		woken := make(chan struct{}, 4)
		p := gatewayplan.New(gatewayplan.Config{
			KeyDir: keyDir, KeySettle: 100 * time.Millisecond, MCPAskURL: "http://koinon-api:8080/mcp",
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			ToolServers: map[string]string{gateway.KoinonTarget: "http://koinon-api:8081/koinon"},
			Wake:        func() { woken <- struct{}{} },
		})
		in := mcpInput()
		in.Allowances = allow
		_, err := p.Begin(in).Settle(context.Background(), []string{"mcp-wiki-f1"}, nil, lk)
		Expect(err).NotTo(HaveOccurred())
		p.Close()
		Consistently(woken, 300*time.Millisecond).ShouldNot(Receive())

		// A pass after Close schedules nothing either.
		_, err = p.Begin(in).Settle(context.Background(), []string{"mcp-wiki-f2"}, nil, lk)
		Expect(err).NotTo(HaveOccurred())
		Consistently(woken, 300*time.Millisecond).ShouldNot(Receive())
	})

	It("keeps the probe keys across passes, so a pass does not revoke them", func() {
		p := newPlanner(0)
		for range 2 {
			in := mcpInput()
			in.Allowances = allow
			d := p.Begin(in)
			settle(d, nil, nil, lk).Applied(gatewayplan.ApplyResult{})
		}
		Expect(lk.issued).To(Equal(2))
	})

	It("asks for a probe of a server that is newly served or whose credential or URL changed, and of none else (old issue 226)", func() {
		p := newPlanner(0)
		apply := func(mut func(*gatewayplan.Input)) []string {
			in := mcpInput()
			in.Allowances = allow
			if mut != nil {
				mut(&in)
			}
			var names []string
			for _, pr := range settle(p.Begin(in), nil, nil, lk).Applied(gatewayplan.ApplyResult{}).Probe {
				names = append(names, pr.Name)
			}
			return names
		}
		Expect(apply(nil)).To(ConsistOf("wiki", "sick"), "every server is new to a fresh process")
		Expect(apply(nil)).To(BeEmpty(), "an unchanged server waits for the ticker")
		Expect(apply(func(in *gatewayplan.Input) {
			in.MCP[0].File = "mcp-wiki-f2"
			in.MCPFiles = map[string][]byte{"mcp-wiki-f2": []byte("koinon_test_not-a-real-token")}
		})).To(ConsistOf("wiki"), "a changed credential")
		Expect(apply(func(in *gatewayplan.Input) { in.MCP[0].URL = "https://wiki2.example.test/mcp" })).To(ConsistOf("wiki"), "a changed URL")

		in := mcpInput()
		in.Allowances = allow
		out := settle(p.Begin(in), nil, nil, lk).Applied(gatewayplan.ApplyResult{Failed: true})
		Expect(out.Probe).To(BeEmpty(), "nothing was applied")
	})

	It("leaves a server out while its new credential settles, and serves it with the previous one", func() {
		p := newPlanner(time.Minute)
		old := mcpInput()
		old.Now = t0.Add(-time.Hour)
		old.Allowances = allow
		d := p.Begin(old)
		settle(d, nil, nil, lk).Applied(gatewayplan.ApplyResult{})

		in := mcpInput()
		in.MCP[0].File = "mcp-wiki-f2"
		in.MCPFiles = map[string][]byte{"mcp-wiki-f2": []byte("koinon_test_not-a-real-token")}
		in.Allowances = allow
		d = p.Begin(in)
		pl := settle(d, []string{"mcp-wiki-f2"}, nil, lk)
		var cred string
		for _, t := range pl.State.MCP {
			if t.Name == "acme-wiki" {
				cred = t.CredentialFile
			}
		}
		Expect(cred).To(Equal(path.Join(keyDir, "mcp-wiki-f1")))
		Expect(pl.Keep).To(And(HaveKey("mcp-wiki-f1"), HaveKey("mcp-wiki-f2")))

		// With no previous file the server is not served at all.
		fresh := newPlanner(time.Minute)
		d = fresh.Begin(in)
		pl = settle(d, []string{"mcp-wiki-f2"}, nil, lk)
		for _, t := range pl.State.MCP {
			Expect(t.Name).NotTo(Equal("acme-wiki"))
		}
	})
})

var _ = Describe("Rendered state", func() {
	// The plan's state is what the gateway renders, so its rendering is the byte-level check that a
	// change of the plan changes nothing the gateway would see (old issue 455: behaviour identical).
	It("matches testdata/plan-state.yaml", func() {
		lk := &fakeLookups{servedFile: map[pgtype.UUID]string{}}
		in := mcpInput()
		in.Orgs = []gatewayplan.Org{acme, globex}
		a2 := agent(acme, 2, "a2")
		a2.Tools = []gateway.ToolGrant{{Target: "github", Tools: []string{"get_issue"}}}
		spent := agent(globex, 3, "spent")
		spent.Limit = &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: budget.DayWindow, Generation: dayGen(t0, "3")}
		in.Agents = append(in.Agents, a2, spent)
		in.Providers = []gatewayplan.Provider{provider(acme, 10, "openrouter", "prov-f1"), provider(globex, 11, "local", "")}
		in.ProviderFiles = map[string][]byte{"prov-f1": []byte("koinon_test_not-a-real-token")}
		in.ModelIDs = map[string]pgtype.UUID{gateway.ModelRef("acme", "open/model"): uuid(50), gateway.ModelRef("globex", "open/model"): uuid(51)}

		in.Allowances = map[string]map[pgtype.UUID][]gatewayplan.Allowed{"acme": {uuid(1): {{Server: uuid(20)}}, uuid(2): {{Server: uuid(20), Tools: []string{"search"}}}}}
		d := newPlanner(0).Begin(in)
		pl := settle(d, []string{"prov-f1"}, nil, lk)
		got, _, err := standalone.Render(pl.State, standalone.Options{DatabaseURL: "sqlite:///data/agentgateway.db?mode=rwc"})
		Expect(err).NotTo(HaveOccurred())

		file := filepath.Join("testdata", "plan-state.yaml")
		if *update {
			Expect(os.WriteFile(file, got, 0o600)).To(Succeed())
		}
		want, err := os.ReadFile(file)
		Expect(err).NotTo(HaveOccurred(), "run with -args -update to create it")
		Expect(string(got)).To(Equal(string(want)))
	})
})
