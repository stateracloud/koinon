// SPDX-License-Identifier: Apache-2.0

package gatewayplan_test

import (
	"io"
	"log/slog"
	"path"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

var _ = Describe("Forge credentials (#1086 part C)", func() {
	var lk *fakeLookups
	BeforeEach(func() { lk = &fakeLookups{} })

	// fileOf is a forge credential file name of agent n of acme, as the reconciler names it, for a token
	// of the scope "github".
	fileIn := func(n byte, scope, hash string) string {
		return forgecredfile.Family(acme.ID, uuid(n), scope) + hash
	}
	fileOf := func(n byte, hash string) string { return fileIn(n, "github", hash) }
	withForge := func(a gatewayplan.Agent, file string) gatewayplan.Agent {
		a.Forge = &gatewayplan.ForgeCredential{Name: "org/acme/agent/" + a.Ref.Name + "/codehost", File: file, Scheme: route.SchemeBasic, User: "x-access-token"}
		return a
	}
	in := func(now time.Time, agents ...gatewayplan.Agent) gatewayplan.Input {
		files := map[string][]byte{}
		for _, a := range agents {
			if a.Forge != nil {
				files[a.Forge.File] = []byte(`{"format":"v2"}`)
			}
		}
		return gatewayplan.Input{Now: now, Orgs: []gatewayplan.Org{acme}, Agents: agents, EgressFiles: files}
	}

	It("names the sealed file of each active agent in the state, and writes and keeps the file", func() {
		f1 := fileOf(1, "aaaaaaaa")
		d := newPlanner(0).Begin(in(t0, withForge(agent(acme, 1, "a1"), f1)))
		Expect(d.Files).To(HaveKey(f1))
		pl := settle(d, nil, nil, lk)
		Expect(pl.State.EgressCredentials).To(Equal([]route.Credential{{
			Name: "org/acme/agent/a1/codehost", Org: "acme", Agent: "a1", File: path.Join(keyDir, f1),
			Scheme: route.SchemeBasic, User: "x-access-token",
		}}))
		Expect(pl.Keep).To(HaveKey(f1))
	})

	It("leaves out the credential of an agent that is not active", func() {
		a := withForge(agent(acme, 1, "a1"), fileOf(1, "aaaaaaaa"))
		a.Active = false
		pl := settle(newPlanner(0).Begin(in(t0, a)), nil, nil, lk)
		Expect(pl.State.EgressCredentials).To(BeEmpty())
	})

	It("leaves a new credential out while its file settles, so its routes fail closed", func() {
		f1 := fileOf(1, "aaaaaaaa")
		pl := settle(newPlanner(time.Minute).Begin(in(t0, withForge(agent(acme, 1, "a1"), f1))), []string{f1}, map[string]bool{f1: true}, lk)
		Expect(pl.State.EgressCredentials).To(BeEmpty())
		Expect(pl.Keep).To(HaveKey(f1))
	})

	It("keeps the previously applied file while a new token's file settles, and keeps both files", func() {
		p := newPlanner(time.Minute)
		f1, f2 := fileOf(1, "aaaaaaaa"), fileOf(1, "bbbbbbbb")
		pl := settle(p.Begin(in(t0.Add(-time.Hour), withForge(agent(acme, 1, "a1"), f1))), nil, nil, lk)
		pl.Applied(gatewayplan.ApplyResult{})

		pl = settle(p.Begin(in(t0, withForge(agent(acme, 1, "a1"), f2))), []string{f2}, nil, lk)
		Expect(pl.State.EgressCredentials).To(HaveLen(1))
		Expect(pl.State.EgressCredentials[0].File).To(Equal(path.Join(keyDir, f1)))
		Expect(pl.Keep).To(And(HaveKey(f1), HaveKey(f2)))

		// Once the new file has settled, it is served and the old one is no longer kept.
		pl = settle(p.Begin(in(t0.Add(2*time.Minute), withForge(agent(acme, 1, "a1"), f2))), nil, nil, lk)
		Expect(pl.State.EgressCredentials[0].File).To(Equal(path.Join(keyDir, f2)))
		Expect(pl.Keep).NotTo(HaveKey(f1))
	})

	It("does not record a file from an Apply that failed", func() {
		p := newPlanner(time.Minute)
		f1, f2 := fileOf(1, "aaaaaaaa"), fileOf(1, "bbbbbbbb")
		pl := settle(p.Begin(in(t0.Add(-time.Hour), withForge(agent(acme, 1, "a1"), f1))), nil, nil, lk)
		pl.Applied(gatewayplan.ApplyResult{Failed: true})

		pl = settle(p.Begin(in(t0, withForge(agent(acme, 1, "a1"), f2))), []string{f2}, nil, lk)
		Expect(pl.State.EgressCredentials).To(BeEmpty())
	})

	It("after a restart falls back to a file of the same agent that the store still holds", func() {
		f0, f1 := fileOf(1, "00000000"), fileOf(1, "aaaaaaaa")
		other := fileOf(2, "ffffffff")
		present := map[string]bool{f0: true, f1: true, other: true}
		pl := settle(newPlanner(time.Minute).Begin(in(t0, withForge(agent(acme, 1, "a1"), f1))), []string{f1}, present, lk)
		Expect(pl.State.EgressCredentials).To(HaveLen(1))
		Expect(pl.State.EgressCredentials[0].File).To(Equal(path.Join(keyDir, f0)), "never another agent's file")
		Expect(pl.Keep).To(And(HaveKey(f0), HaveKey(f1), Not(HaveKey(other))))
	})

	It("never keeps serving a file of another scope while the new one settles", func() {
		p := newPlanner(time.Minute)
		old, next := fileIn(1, "github", "aaaaaaaa"), fileIn(1, "gitea", "bbbbbbbb")
		pl := settle(p.Begin(in(t0.Add(-time.Hour), withForge(agent(acme, 1, "a1"), old))), nil, nil, lk)
		pl.Applied(gatewayplan.ApplyResult{})

		// The agent's binding moved to another connection or host: the old token must not go there.
		pl = settle(p.Begin(in(t0, withForge(agent(acme, 1, "a1"), next))), []string{next}, map[string]bool{old: true, next: true}, lk)
		Expect(pl.State.EgressCredentials).To(BeEmpty())
		Expect(pl.Keep).NotTo(HaveKey(old))
	})

	It("after a restart never falls back to a file of another scope", func() {
		other, next := fileIn(1, "github", "ffffffff"), fileIn(1, "gitea", "00000000")
		pl := settle(newPlanner(time.Minute).Begin(in(t0, withForge(agent(acme, 1, "a1"), next))), []string{next}, map[string]bool{other: true, next: true}, lk)
		Expect(pl.State.EgressCredentials).To(BeEmpty())
	})

	It("mounts the files where the MCP credentials are, apart from the provider keys", func() {
		Expect(gatewayplan.IsMCPFile(fileOf(1, "aaaaaaaa"))).To(BeTrue())
		p := gatewayplan.New(gatewayplan.Config{KeyDir: keyDir, MCPKeyDir: "/secrets/mcp", Logger: newPlannerLogger()})
		pl := settle(p.Begin(in(t0, withForge(agent(acme, 1, "a1"), fileOf(1, "aaaaaaaa")))), nil, nil, lk)
		Expect(pl.State.EgressCredentials[0].File).To(Equal(path.Join("/secrets/mcp", fileOf(1, "aaaaaaaa"))))
	})
})

func newPlannerLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
