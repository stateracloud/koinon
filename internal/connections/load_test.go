// SPDX-License-Identifier: Apache-2.0

package connections_test

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// refusingOpener fails the spec when anything is decrypted.
type refusingOpener struct{}

func (refusingOpener) Open(secretstore.Binding, secretstore.Envelope) ([]byte, error) {
	Fail("Load opened an envelope")
	return nil, nil
}

var _ = Describe("Load", func() {
	It("refuses a connection whose credential was lost, with what to do, and opens nothing", func() {
		c := store.Connection{
			OrgID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, ID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
			Connector: "github", DisplayName: "GitHub",
			CredentialLostAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		_, err := connections.Load(context.Background(), refusingOpener{}, c)
		Expect(err).To(MatchError(connections.ErrCredentialLost))
		Expect(err.Error()).To(ContainSubstring(`enter the secret of the connection "GitHub" again on the Connections page`))

		By("a declared connection comes back from its Secret")
		c.Source, c.ManagedBySecret = store.ConfigSourceSecret, pgtype.Text{String: "koinon/github-app", Valid: true}
		_, err = connections.Load(context.Background(), refusingOpener{}, c)
		Expect(err).To(MatchError(connections.ErrCredentialLost))
		Expect(err.Error()).To(ContainSubstring(`the Kubernetes Secret koinon/github-app that declares the connection "GitHub" stores it again at the next sync`))
	})
})
