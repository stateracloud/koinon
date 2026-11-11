// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The checkpoint, seed, resync and sweep ticks list only the orgs with work (#576, as the claim loop
// #378 and the storage probes). Each query is a superset of what its tick does: the claim re-checks
// every condition. No pool runs here; the specs read the lists.
var _ = Describe("agentd org lists for the checkpoint ticks", func() {
	var e *env
	exec := func(ctx SpecContext, sql string, args ...any) {
		GinkgoHelper()
		_, err := db.Exec(ctx, sql, args...)
		Expect(err).NotTo(HaveOccurred())
	}
	contains := func(list func() ([]pgtype.UUID, error)) func() bool {
		return func() bool {
			orgs, err := list()
			Expect(err).NotTo(HaveOccurred())
			for _, o := range orgs {
				if o == e.org {
					return true
				}
			}
			return false
		}
	}
	deleteOrg := func(ctx SpecContext) {
		GinkgoHelper()
		exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, e.org)
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, store.AgentFlavorOpencode)
	})

	Describe("checkpoints", func() {
		interval := int32(time.Hour.Seconds())
		listed := func(ctx SpecContext) func() bool {
			return contains(func() ([]pgtype.UUID, error) { return q.ListOrgIDsWithCheckpointWork(ctx, interval) })
		}

		It("lists an org whose ready sandbox has had no checkpoint for the interval, and not one that has", func(ctx SpecContext) {
			Expect(listed(ctx)()).To(BeTrue(), "an hourly request is due")
			exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, size_bytes, generation, epoch, started_at, finished_at)
				VALUES ($1, $2, 'hourly', 'complete', 'snap', 1, 'g', 0, now(), now())`, e.org, e.agent.ID)
			Expect(listed(ctx)()).To(BeFalse(), "a checkpoint within the interval: nothing is due")
			exec(ctx, `UPDATE home_checkpoints SET requested_at = now() - interval '2 hours' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "the last checkpoint is older than the interval")
		})

		It("lists an org with an open request, due or not, and drops it once the request ends", func(ctx SpecContext) {
			exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason) VALUES ($1, $2, 'hourly')`, e.org, e.agent.ID)
			Expect(listed(ctx)()).To(BeTrue(), "a requested checkpoint")
			exec(ctx, `UPDATE agent_sandboxes SET state = 'stopped' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "a request whose sandbox is not ready is still the tick's to fail")
			exec(ctx, `UPDATE home_checkpoints SET status = 'failed', error = 'x', finished_at = now() WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "no open request and no ready sandbox")
		})

		It("does not list an org whose sandbox is not ready, or unhealthy", func(ctx SpecContext) {
			exec(ctx, `UPDATE agent_sandboxes SET state = 'stopped' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "a stopped sandbox")
			exec(ctx, `UPDATE agent_sandboxes SET state = 'ready', unhealthy_at = now(), unhealthy_reason = 'stuck', unhealthy_generation = 'g' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "an unhealthy sandbox")
		})

		It("does not list an org whose only agent is archived or ephemeral (no hourly checkpoint can be made)", func(ctx SpecContext) {
			exec(ctx, `UPDATE agents SET status = 'archived', archived_at = now() WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "an archived agent")
			exec(ctx, `UPDATE agents SET status = 'active', archived_at = NULL WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "an active agent is due")
			exec(ctx, `UPDATE agent_versions SET config = config || '{"ephemeral": true}'::jsonb
				WHERE org_id = $1 AND id = (SELECT active_version_id FROM agents WHERE org_id = $1 AND id = $2)`, e.org, e.agent.ID)
			Expect(listed(ctx)()).To(BeFalse(), "an ephemeral agent")
		})

		It("does not list a deleted org", func(ctx SpecContext) {
			Expect(listed(ctx)()).To(BeTrue())
			deleteOrg(ctx)
			Expect(listed(ctx)()).To(BeFalse())
		})
	})

	Describe("seeds", func() {
		listed := func(ctx SpecContext) func() bool {
			return contains(func() ([]pgtype.UUID, error) { return q.ListOrgIDsWithSeedWork(ctx) })
		}
		// A repository epoch 1 that waits for its seed from epoch 0 (what keys new-repositories leaves).
		newEpoch := func(ctx SpecContext) {
			GinkgoHelper()
			exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
				e.org, e.agent.ID, checkpoint.Prefix(e.org.String(), e.agent.ID.String()))
			exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, seed_from_epoch, seed_after)
				VALUES ($1, $2, 1, $3, '{}', 'key_compromised', 0, now())`,
				e.org, e.agent.ID, checkpoint.EpochPrefix(e.org.String(), e.agent.ID.String(), 1))
		}

		It("lists an org with a pending seed, due or not, and drops it once seeded", func(ctx SpecContext) {
			Expect(listed(ctx)()).To(BeFalse(), "no seed")
			newEpoch(ctx)
			Expect(listed(ctx)()).To(BeTrue(), "a due seed")
			exec(ctx, `UPDATE home_repository_epochs SET seed_after = now() + interval '1 hour' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "a seed not yet due still counts in the pending gauge")
			exec(ctx, `UPDATE home_repository_epochs SET seeded_at = now() WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "seeded")
		})

		It("keeps an org whose seed was given up on (the failed gauge) and drops a superseded one", func(ctx SpecContext) {
			newEpoch(ctx)
			exec(ctx, `UPDATE home_repository_epochs SET seed_failed_at = now(), seed_error = 'x' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "given up on")
			exec(ctx, `UPDATE home_repository_epochs SET retired_at = now() WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "superseded")
		})

		It("does not list a deleted org", func(ctx SpecContext) {
			newEpoch(ctx)
			Expect(listed(ctx)()).To(BeTrue())
			deleteOrg(ctx)
			Expect(listed(ctx)()).To(BeFalse())
		})
	})

	Describe("resyncs", func() {
		listed := func(ctx SpecContext) func() bool {
			return contains(func() ([]pgtype.UUID, error) { return q.ListOrgIDsWithResyncWork(ctx) })
		}
		BeforeEach(func(ctx SpecContext) {
			exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
				e.org, e.agent.ID, checkpoint.Prefix(e.org.String(), e.agent.ID.String()))
		})

		It("lists an org with a pending resync, due or not, and drops it once finished", func(ctx SpecContext) {
			Expect(listed(ctx)()).To(BeFalse(), "no resync")
			_, err := q.RequestHomeCheckpointResync(ctx, store.RequestHomeCheckpointResyncParams{OrgID: e.org, AgentID: e.agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(listed(ctx)()).To(BeTrue(), "a due resync")
			exec(ctx, `UPDATE home_checkpoint_resyncs SET resync_after = now() + interval '1 hour' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "a resync under its lease or retry delay still counts in the pending gauge")
			exec(ctx, `UPDATE home_checkpoint_resyncs SET failed_at = now(), error = 'x' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "given up on (the failed gauge)")
			exec(ctx, `UPDATE home_checkpoint_resyncs SET failed_at = NULL, error = NULL, finished_at = now(), snapshots = 0, added = 0, missing = 0, skipped = 0, taken = 0 WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "finished")
		})

		It("does not list a deleted org", func(ctx SpecContext) {
			_, err := q.RequestHomeCheckpointResync(ctx, store.RequestHomeCheckpointResyncParams{OrgID: e.org, AgentID: e.agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(listed(ctx)()).To(BeTrue())
			deleteOrg(ctx)
			Expect(listed(ctx)()).To(BeFalse())
		})
	})

	Describe("the repository sweep", func() {
		listed := func(ctx SpecContext) func() bool {
			return contains(func() ([]pgtype.UUID, error) { return q.ListOrgIDsWithRepositoryDeleteWork(ctx) })
		}
		repo0 := func(ctx SpecContext) {
			GinkgoHelper()
			exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
				e.org, e.agent.ID, checkpoint.Prefix(e.org.String(), e.agent.ID.String()))
		}

		It("lists an org with a repository past its deadline, not one before it or already deleted", func(ctx SpecContext) {
			repo0(ctx)
			Expect(listed(ctx)()).To(BeFalse(), "a live repository has no deadline")
			exec(ctx, `UPDATE home_repositories SET retired_at = now(), delete_after = now() + interval '1 hour' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "before the deadline")
			exec(ctx, `UPDATE home_repositories SET delete_after = now() - interval '1 second' WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeTrue(), "past the deadline")
			exec(ctx, `UPDATE home_repositories SET deleted_at = now() WHERE org_id = $1`, e.org)
			Expect(listed(ctx)()).To(BeFalse(), "deleted")
		})

		It("lists an org with a retired epoch past its deadline, and not a deleted org", func(ctx SpecContext) {
			repo0(ctx)
			exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, retired_at, delete_after)
				VALUES ($1, $2, 1, $3, '{}', 'key_compromised', now(), now() - interval '1 second')`,
				e.org, e.agent.ID, checkpoint.EpochPrefix(e.org.String(), e.agent.ID.String(), 1))
			Expect(listed(ctx)()).To(BeTrue())
			deleteOrg(ctx)
			Expect(listed(ctx)()).To(BeFalse())
		})
	})
})
