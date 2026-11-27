// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("AgentSandbox.ActorRef", func() {
	It("names the sandbox when no actor name is recorded", func() {
		Expect(store.AgentSandbox{SandboxName: "ka-0123456789abcdef"}.ActorRef()).To(Equal("ka-0123456789abcdef"))
		Expect(store.AgentSandbox{SandboxName: "ka-0123456789abcdef", ActorName: pgtype.Text{Valid: true}}.ActorRef()).
			To(Equal("ka-0123456789abcdef"))
	})

	It("names the actor when one is recorded", func() {
		sb := store.AgentSandbox{SandboxName: "ka-0123456789abcdef", ActorName: pgText("ka-0123456789abcdef-0a1b2c3d")}
		Expect(sb.ActorRef()).To(Equal("ka-0123456789abcdef-0a1b2c3d"))
	})
})

var _ = Describe("sandbox actor record (issue #1386)", func() {
	var (
		f  fixture
		sb store.AgentSandbox
	)

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		ws := uniq("ws")
		_, err := q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{OrgID: f.org, Workspace: ws})
		Expect(err).NotTo(HaveOccurred())
		name := uniq("ka")
		sb, err = q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
			OrgID: f.org, AgentID: f.agent.ID, Workspace: ws, SandboxName: name, ProviderName: name, KeySecretName: name,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.ActorName.Valid).To(BeFalse())
		Expect(sb.ActorUid.Valid).To(BeFalse())
	})

	setForge := func(ctx context.Context, rev int64) {
		Expect(q.SetGatewayAgentForge(ctx, store.SetGatewayAgentForgeParams{
			OrgID: f.org, AgentID: f.agent.ID, StateRevision: pgtype.Int8{Int64: rev, Valid: true},
		})).To(Succeed())
		Expect(q.MarkGatewayAgentForgeServed(ctx, store.MarkGatewayAgentForgeServedParams{
			OrgID: f.org, AgentID: f.agent.ID, StateRevision: pgtype.Int8{Int64: rev, Valid: true},
		})).To(Succeed())
	}

	It("names a new actor with no UID, and records the UID of the observed actor", func(ctx SpecContext) {
		row, err := q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d"})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.ActorRef()).To(Equal(sb.SandboxName + "-0a1b2c3d"))
		Expect(row.ActorUid.Valid).To(BeFalse())

		row, err = q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
			OrgID: f.org, AgentID: f.agent.ID, ActorName: row.ActorName.String, ActorUid: "uid-1",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.ActorUid).To(Equal(pgText("uid-1")))

		By("a new name clears the UID of the old actor")
		row, err = q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-4e5f6a7b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.ActorName).To(Equal(pgText(sb.SandboxName + "-4e5f6a7b")))
		Expect(row.ActorUid.Valid).To(BeFalse())
	})

	It("refuses a UID for a name that is not the recorded one, and changes nothing for the same UID", func(ctx SpecContext) {
		_, err := q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
			OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-ffffffff", ActorUid: "uid-1",
		})
		Expect(errors.Is(err, pgx.ErrNoRows)).To(BeTrue())

		_, err = q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
			OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d", ActorUid: "uid-1",
		})
		Expect(err).NotTo(HaveOccurred())
		setForge(ctx, 7)
		_, err = q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
			OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d", ActorUid: "uid-1",
		})
		Expect(errors.Is(err, pgx.ErrNoRows)).To(BeTrue())
		forge, err := q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: f.org, AgentID: f.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(forge.StateRevision).To(Equal(pgtype.Int8{Int64: 7, Valid: true}))
		Expect(forge.ServedAt.Valid).To(BeTrue())
	})

	It("resets the forge row when a new UID is recorded, so the run waits for the actor", func(ctx SpecContext) {
		_, err := q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d"})
		Expect(err).NotTo(HaveOccurred())
		setForge(ctx, 7)
		_, err = q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
			OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d", ActorUid: "uid-1",
		})
		Expect(err).NotTo(HaveOccurred())
		forge, err := q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: f.org, AgentID: f.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(forge.StateRevision.Valid).To(BeFalse())
		Expect(forge.ServedAt.Valid).To(BeFalse())
	})

	It("resets the forge row when a new actor is named, so no run passes on the old actor", func(ctx SpecContext) {
		_, err := q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
			OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d", ActorUid: "uid-1",
		})
		Expect(err).NotTo(HaveOccurred())
		setForge(ctx, 7)
		_, err = q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-4e5f6a7b"})
		Expect(err).NotTo(HaveOccurred())
		forge, err := q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: f.org, AgentID: f.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(forge.StateRevision.Valid).To(BeFalse())
		Expect(forge.ServedAt.Valid).To(BeFalse())
	})

	It("clears the actor when the row moves to the sandbox of the next task", func(ctx SpecContext) {
		_, err := q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: sb.SandboxName + "-0a1b2c3d"})
		Expect(err).NotTo(HaveOccurred())
		next := uniq("kt")
		row, err := q.SetAgentSandboxName(ctx, store.SetAgentSandboxNameParams{OrgID: f.org, AgentID: f.agent.ID, SandboxName: next})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.ActorName.Valid).To(BeFalse())
		Expect(row.ActorUid.Valid).To(BeFalse())
		Expect(row.ActorRef()).To(Equal(next))
	})

	It("refuses an actor name that is not a DNS label", func(ctx SpecContext) {
		_, err := q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{OrgID: f.org, AgentID: f.agent.ID, ActorName: "Not_A-Label"})
		Expect(err).To(HaveOccurred())
	})
})
