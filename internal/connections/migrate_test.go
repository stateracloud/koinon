// SPDX-License-Identifier: Apache-2.0

package connections_test

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

type memLegacy struct {
	secrets   map[string]connections.Secret
	deleteErr error
}

func (m *memLegacy) Create(_ context.Context, s connections.Secret) error {
	m.secrets[s.Name] = s
	return nil
}

func (m *memLegacy) Get(_ context.Context, name string) (connections.Secret, bool, error) {
	s, ok := m.secrets[name]
	return s, ok, nil
}

func (m *memLegacy) Delete(_ context.Context, name string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.secrets, name)
	return nil
}

var _ = Describe("MigrateLegacy", func() {
	var (
		q      *store.Queries
		legacy *memLegacy
		org    pgtype.UUID
	)
	BeforeEach(func(ctx SpecContext) {
		_, err := pool.Exec(ctx, `TRUNCATE orgs CASCADE`)
		Expect(err).NotTo(HaveOccurred())
		q = store.New(pool)
		legacy = &memLegacy{secrets: map[string]connections.Secret{}}
		org, _, _ = pgtest.NewOrg(ctx, pool)
	})

	oldConnection := func(ctx SpecContext, pem string, withSecret bool) store.Connection {
		GinkgoHelper()
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		name := connections.SecretName(org.Bytes, id.Bytes)
		if withSecret {
			legacy.secrets[name] = connections.Secret{Name: name, Data: map[string][]byte{connections.GitHubPrivateKeyField: []byte(pem)}}
		}
		c, err := q.CreateConnection(ctx, store.CreateConnectionParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "old", Config: []byte(`{"appId":1,"installationId":2}`), SecretName: pgtype.Text{String: name, Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		return c
	}

	It("seals the old Secret's fields into the row, then deletes the Secret", func(ctx SpecContext) {
		ring := secretstoretest.Keyring()
		c := oldConnection(ctx, "the-pem", true)
		n, err := connections.MigrateLegacy(ctx, q, legacy, ring, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(legacy.secrets).To(BeEmpty())

		got, err := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: c.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.SecretName.Valid).To(BeFalse(), "cleared once the old Secret was deleted")
		fields, err := connections.OpenFields(ring, got)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fields[connections.GitHubPrivateKeyField])).To(Equal("the-pem"))
		Expect(string(got.SecretEnvelope)).NotTo(ContainSubstring("the-pem"))

		n, err = connections.MigrateLegacy(ctx, q, legacy, ring, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero(), "idempotent")
	})

	It("keeps the old Secret when sealing fails, and tries again next start", func(ctx SpecContext) {
		c := oldConnection(ctx, "the-pem", true)
		_, err := connections.MigrateLegacy(ctx, q, legacy, nil, slog.New(slog.DiscardHandler))
		Expect(err).To(MatchError(connections.ErrNoKeyring))
		Expect(legacy.secrets).To(HaveLen(1))
		got, _ := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: c.ID})
		Expect(got.SecretName.Valid).To(BeTrue())
		Expect(got.SecretEnvelope).To(BeEmpty())

		n, err := connections.MigrateLegacy(ctx, q, legacy, secretstoretest.Keyring(), slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
	})

	It("moves the fields even when the old Secret cannot be deleted, keeps secret_name, and finishes at the next start", func(ctx SpecContext) {
		c := oldConnection(ctx, "the-pem", true)
		ring := secretstoretest.Keyring()
		legacy.deleteErr = errors.New("forbidden")
		var logs capture
		n, err := connections.MigrateLegacy(ctx, q, legacy, ring, slog.New(slog.NewTextHandler(&logs, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(logs.String()).To(ContainSubstring("could not delete its Kubernetes Secret"))
		Expect(logs.String()).NotTo(ContainSubstring("the-pem"))
		got, _ := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: c.ID})
		Expect(got.SecretEnvelope).NotTo(BeEmpty())
		Expect(got.SecretName.Valid).To(BeTrue(), "the name stays while the Secret does")
		Expect(legacy.secrets).To(HaveLen(1))

		legacy.deleteErr = nil
		n, err = connections.MigrateLegacy(ctx, q, legacy, ring, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero(), "the fields were not sealed twice")
		Expect(legacy.secrets).To(BeEmpty())
		got, _ = q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: c.ID})
		Expect(got.SecretName.Valid).To(BeFalse())
		fields, err := connections.OpenFields(ring, got)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fields[connections.GitHubPrivateKeyField])).To(Equal("the-pem"))
	})

	It("finishes a move whose Secret is already gone by clearing the name", func(ctx SpecContext) {
		ring := secretstoretest.Keyring()
		c := oldConnection(ctx, "x", true)
		_, err := connections.MigrateLegacy(ctx, q, &memLegacy{secrets: legacy.secrets, deleteErr: errors.New("no")}, ring, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		legacy.secrets = map[string]connections.Secret{} // someone deleted it by hand
		_, err = connections.MigrateLegacy(ctx, q, legacy, ring, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		got, _ := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: c.ID})
		Expect(got.SecretName.Valid).To(BeFalse())
	})

	It("skips a connection whose Secret is already gone and migrates the others", func(ctx SpecContext) {
		oldConnection(ctx, "", false)
		oldConnection(ctx, "second-pem", true)
		n, err := connections.MigrateLegacy(ctx, q, legacy, secretstoretest.Keyring(), slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
	})

	It("says so when connections wait for a Kubernetes Secret and there is no access to it", func(ctx SpecContext) {
		oldConnection(ctx, "x", true)
		_, err := connections.MigrateLegacy(ctx, q, nil, secretstoretest.Keyring(), slog.New(slog.DiscardHandler))
		Expect(err).To(MatchError(ContainSubstring("no Kubernetes access")))
	})
})

type capture struct{ b []byte }

func (c *capture) Write(p []byte) (int, error) { c.b = append(c.b, p...); return len(p), nil }
func (c *capture) String() string              { return string(c.b) }
