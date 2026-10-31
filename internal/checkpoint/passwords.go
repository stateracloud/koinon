// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Passwords holds each agent's repository password, sealed with the secret store and bound to the
// home_repositories row, so a copy in another row, column or org does not open. The password is
// random and created with the repository; only the writer opens it, and it is never logged.
type Passwords struct {
	Q    *store.Queries
	Keys *secretstore.Keyring
}

// Repository is where an agent's home is saved and the password that opens it.
type Repository struct {
	Prefix   string
	Password string
}

// The table and column that hold the sealed repository passwords. The table is keyed by agent_id,
// not id, which koinon-api keys (internal/keyscli) knows.
const (
	Table  = "home_repositories"
	Column = "password_envelope"
)

// Binding is the AAD binding of an agent's repository password (the trace router reads it to know
// the values to scrub).
func Binding(org, agent pgtype.UUID) secretstore.Binding { return binding(org, agent) }

func binding(org, agent pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: Table, RowID: agent.String(), Column: Column}
}

// Get returns the agent's repository, creating its password on first use.
func (p Passwords) Get(ctx context.Context, org, agent pgtype.UUID) (Repository, error) {
	row, err := p.Q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = p.create(ctx, org, agent)
	}
	if err != nil {
		return Repository{}, err
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(row.PasswordEnvelope, &env); err != nil {
		return Repository{}, fmt.Errorf("the repository password of agent %s is unreadable: %w", agent, err)
	}
	pw, err := p.Keys.Open(binding(org, agent), env)
	if err != nil {
		return Repository{}, fmt.Errorf("opening the repository password of agent %s: %w", agent, err)
	}
	return Repository{Prefix: row.Prefix, Password: string(pw)}, nil
}

func (p Passwords) create(ctx context.Context, org, agent pgtype.UUID) (store.HomeRepository, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return store.HomeRepository{}, err
	}
	env, err := p.Keys.Seal(binding(org, agent), []byte(base64.RawURLEncoding.EncodeToString(raw)))
	if err != nil {
		return store.HomeRepository{}, err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return store.HomeRepository{}, err
	}
	row, err := p.Q.InsertHomeRepository(ctx, store.InsertHomeRepositoryParams{
		OrgID: org, AgentID: agent, Prefix: Prefix(org.String(), agent.String()), PasswordEnvelope: b,
	})
	if errors.Is(err, pgx.ErrNoRows) { // created by another writer in the meantime
		return p.Q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: agent})
	}
	return row, err
}
