// SPDX-License-Identifier: Apache-2.0

package restorestatus_test

import (
	"bytes"
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/restorestatus"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Build", func() {
	It("a BestEffort build keeps going when a section that gates nothing cannot be read; the importer section still fails", func(ctx SpecContext) {
		_, err := pool.Exec(ctx, `ALTER TABLE platform_trace_destination RENAME TO platform_trace_destination_x`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) {
			_, err := pool.Exec(ctx, `ALTER TABLE platform_trace_destination_x RENAME TO platform_trace_destination`)
			Expect(err).NotTo(HaveOccurred())
		})
		_, err = restorestatus.Build(ctx, pool, "own-mark-hash")
		Expect(err).To(HaveOccurred(), "the full status fails on the unreadable section")
		r, err := restorestatus.Build(ctx, pool, "own-mark-hash", restorestatus.Options{BestEffort: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Warnings).To(ConsistOf(ContainSubstring("outward targets")))
	})

	It("reports a copy: the credentials it refuses, the outward targets and the work a release starts", func(ctx SpecContext) {
		for _, stmt := range []string{`DELETE FROM install_binding`, `DELETE FROM api_tokens`, `DELETE FROM sessions`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
		org, user, _ := pgtest.NewOrg(ctx, pool)
		_, err := pool.Exec(ctx, `INSERT INTO install_binding (mark, public_url) VALUES ('source-mark-hash', 'https://source.example')`)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, auth_method, created_at, last_seen_at, idle_expires_at, expires_at, install_mark)
			VALUES (sha256('a'), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '2 hours', 'source-mark-hash'),
			       (sha256('b'), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '2 hours', 'own-mark-hash')`, user)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO api_tokens (org_id, name, prefix, token_hash, role, created_by, expires_at, install_mark)
			VALUES ($1, 'ci', 'abcdefgh', sha256('t'), 'admin', $2, now() + interval '1 day', 'source-mark-hash')`, org, user)
		Expect(err).NotTo(HaveOccurred())

		r, err := restorestatus.Build(ctx, pool, "own-mark-hash")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Binding.State).To(Equal("copy"))
		Expect(r.Binding.RecordedHost.PublicURL).To(Equal("https://source.example"))
		Expect(r.Quarantine.Tokens).To(Equal(1))
		Expect(r.Quarantine.Sessions).To(Equal(1), "a session made on the copy (its own mark) is accepted")

		var out bytes.Buffer
		restorestatus.Print(&out, r)
		Expect(out.String()).To(ContainSubstring("binding:    copy"))
		Expect(strings.Count(out.String(), "refused:")).To(Equal(1))

		By("without a held record the dump time is unknown and every active token is listed")
		Expect(r.DumpTokens.DumpTime).To(BeNil())
		Expect(r.DumpTokens.Tokens).To(HaveLen(1))
		Expect(r.DumpTokens.Tokens[0].Prefix).To(Equal("abcdefgh"))

		By("a configured host that equals the source's recorded one is marked")
		r, err = restorestatus.Build(ctx, pool, "own-mark-hash", restorestatus.Options{Configured: restorestatus.ConfiguredTargets{
			PublicURL: "https://source.example", OIDCRedirectURL: "https://copy.example",
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Hosts).To(ConsistOf(
			restorestatus.HostTarget{Name: "public-url", Value: "https://source.example", EqualsSource: true},
			restorestatus.HostTarget{Name: "oidc-redirect-url", Value: "https://copy.example"},
		))

		By("with held_since the dump time is the newest event before it; a token made after it is left out")
		_, err = audit.InPgTx(ctx, pool, func(_ pgx.Tx, ab audit.Batch) error {
			ab.Install(audit.InstallEvent{Actor: audit.System("test"), Action: "test.marker", TargetKind: "install", TargetID: "install_binding"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE install_binding SET held_since = now() + interval '1 minute'`)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO api_tokens (org_id, name, prefix, token_hash, role, created_by, created_at, expires_at)
			VALUES ($1, 'later', 'ijklmnop', sha256('u'), 'admin', $2, now() + interval '2 minutes', now() + interval '1 day')`, org, user)
		Expect(err).NotTo(HaveOccurred())
		r, err = restorestatus.Build(ctx, pool, "own-mark-hash")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.DumpTokens.DumpTime).NotTo(BeNil())
		Expect(r.DumpTokens.Tokens).To(HaveLen(1), "the token made after the dump time is left out")
		Expect(r.DumpTokens.Tokens[0].Prefix).To(Equal("abcdefgh"))

		By("without a mark the state is unknown and nothing is counted")
		r, err = restorestatus.Build(ctx, pool, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Binding.State).To(Equal("unknown"))
		Expect(r.Quarantine.Tokens + r.Quarantine.Sessions).To(BeZero())
	})

	It("does not list a removed target as in use (#940)", func(ctx SpecContext) {
		_, err := pool.Exec(ctx, `DELETE FROM install_binding`)
		Expect(err).NotTo(HaveOccurred())
		org, _, _ := pgtest.NewOrg(ctx, pool)
		_, err = pool.Exec(ctx, `INSERT INTO connections (org_id, connector, display_name, secret_name, source, managed_by_secret, removed_at)
			VALUES ($1, 'github', 'gone-connection', 'gone-conn-secret', 'secret', 'gone', now()),
			       ($1, 'github', 'live-connection', 'live-conn-secret', 'secret', 'live', NULL)`, org)
		Expect(err).NotTo(HaveOccurred())
		r, err := restorestatus.Build(ctx, pool, "own-mark-hash")
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, t := range r.Targets {
			names = append(names, t.Name)
		}
		Expect(names).To(ContainElement("live-connection"))
		Expect(names).NotTo(ContainElement("gone-connection"))
	})

	It("reports whether a release is pending, the importer's removals and the org limits (ADR 0110 decisions 1 and 7)", func(ctx SpecContext) {
		for _, stmt := range []string{`DELETE FROM install_binding`, `DELETE FROM restore_accepted_removals`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
		org, _, slug := pgtest.NewOrg(ctx, pool)
		_, err := pool.Exec(ctx, `INSERT INTO install_binding (mark, held_since) VALUES (NULL, now())`)
		Expect(err).NotTo(HaveOccurred())
		gone := installbinding.Removal{Kind: "model-provider", Org: slug, Secret: "source-provider"}
		plan := func(context.Context) ([]installbinding.Removal, error) { return []installbinding.Removal{gone}, nil }

		r, err := restorestatus.Build(ctx, pool, "own-mark-hash", restorestatus.Options{PlanRemovals: plan})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Release.Pending).To(BeTrue())
		Expect(r.Importer.Planned).To(ConsistOf(gone))
		Expect(r.Importer.NeedsAccept).To(BeTrue())
		Expect(r.Blocking()).To(ConsistOf(gone))
		Expect(r.Release.OrgLimits).To(HaveKey(org.String()))
		Expect(r.Release.OrgLimits[org.String()].MaxConcurrentRuns).To(BeNumerically(">", 0))
		Expect(r.Release.DueTriggers).NotTo(BeNil())

		By("with the accept value nothing blocks")
		r, err = restorestatus.Build(ctx, pool, "own-mark-hash", restorestatus.Options{PlanRemovals: plan, AcceptImporterDeletions: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Blocking()).To(BeEmpty())

		By("after the release the removals wait; the check reports them and does not block the upgrade (#940)")
		_, err = installbinding.New(pool, "own-mark-hash", false, nil).Settle(ctx, []installbinding.Removal{gone}, false)
		Expect(err).NotTo(HaveOccurred())
		r, err = restorestatus.Build(ctx, pool, "own-mark-hash", restorestatus.Options{PlanRemovals: plan})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Release.Pending).To(BeFalse())
		Expect(r.Importer.Waiting).To(ConsistOf(gone))
		Expect(r.Importer.NeedsAccept).To(BeTrue(), "still reported as waiting")
		Expect(r.Blocking()).To(BeEmpty(), "waiting rows do not fail a later helm upgrade")
		r, err = restorestatus.Build(ctx, pool, "own-mark-hash", restorestatus.Options{PlanRemovals: func(context.Context) ([]installbinding.Removal, error) { return nil, nil }})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Importer.NeedsAccept).To(BeFalse(), "the Secret came back: nothing waits for the accept")
	})
})
