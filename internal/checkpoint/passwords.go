// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Passwords holds each agent's repository passwords, sealed with the secret store and bound to
// their row, so a copy in another row, column or org does not open. A password is random and
// created with its repository; only the writer opens it, and it is never logged.
//
// An agent has one repository per repository epoch (ADR 0085 decision 13). Epoch 0 is the
// home_repositories row, at Prefix; each later one is a home_repository_epochs row, at
// [EpochPrefix], started by [Passwords.NewEpoch] when a key-encryption key is lost or leaked. The
// highest epoch is the current one: checkpoints are written there, and each checkpoint records
// its epoch, so a restore opens the repository it was written to.
type Passwords struct {
	Q    *store.Queries
	Keys *secretstore.Keyring
	// Begin starts the transaction of NewEpoch. Nil: NewEpoch is not available.
	Begin func(context.Context) (pgx.Tx, error)
}

// Repository is where an agent's home is saved and the password that opens it.
type Repository struct {
	// Epoch is the repository epoch: 0 for the agent's first repository.
	Epoch    int32
	Prefix   string
	Password string
}

// The tables and column that hold the sealed repository passwords. home_repositories is keyed by
// agent_id, not id, and home_repository_epochs by its binding_row; koinon-api keys
// (internal/keyscli) knows both.
const (
	Table       = "home_repositories"
	EpochsTable = "home_repository_epochs"
	Column      = "password_envelope"
)

// Reasons a repository epoch starts (home_repository_epochs.reason).
const (
	ReasonKeyLost        = "key_lost"
	ReasonKeyCompromised = "key_compromised"
)

// ErrNoRepository is returned for an epoch the agent does not have.
var ErrNoRepository = errors.New("checkpoint: the agent has no repository of that epoch")

// ErrRepositoryLost is returned for a repository whose password was lost with the key-encryption
// key that sealed it (credential_lost_at): its checkpoints cannot be read.
var ErrRepositoryLost = errors.New("checkpoint: the repository's password was lost with its key-encryption key")

// DefaultRetain is how long a retired repository's blobs are kept before the sweeper deletes them
// (ADR 0085 decision 13), unless the org sets its own.
const DefaultRetain = 7 * 24 * time.Hour

// Binding is the AAD binding of an agent's epoch-0 repository password (the trace router reads it
// to know the values to scrub).
func Binding(org, agent pgtype.UUID) secretstore.Binding { return BindingAt(org, agent, 0) }

// BindingAt is the AAD binding of the password of the agent's repository of the given epoch.
func BindingAt(org, agent pgtype.UUID, epoch int32) secretstore.Binding {
	if epoch == 0 {
		return secretstore.Binding{OrgID: org.String(), Table: Table, RowID: agent.String(), Column: Column}
	}
	return EpochRowBinding(org, EpochRow(agent.String(), epoch))
}

// EpochRowBinding is the binding of the password in the home_repository_epochs row whose
// binding_row is row (the trace router reads the rows by that key).
func EpochRowBinding(org pgtype.UUID, row string) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: EpochsTable, RowID: row, Column: Column}
}

// EpochRow is home_repository_epochs.binding_row: the agent and the epoch.
func EpochRow(agent string, epoch int32) string {
	return agent + "/r" + strconv.FormatInt(int64(epoch), 10)
}

// Get returns the agent's current repository, creating its first one on first use.
func (p Passwords) Get(ctx context.Context, org, agent pgtype.UUID) (Repository, error) {
	return p.Current(ctx, org, agent)
}

// Current returns the agent's current repository: its highest epoch, or epoch 0, which it creates
// on first use.
func (p Passwords) Current(ctx context.Context, org, agent pgtype.UUID) (Repository, error) {
	e, err := p.Q.LatestHomeRepositoryEpoch(ctx, store.LatestHomeRepositoryEpochParams{OrgID: org, AgentID: agent})
	if err == nil {
		return p.open(org, agent, e.RepositoryEpoch, e.Prefix, e.PasswordEnvelope)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, err
	}
	row, err := p.Q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = p.create(ctx, org, agent)
	}
	if err != nil {
		return Repository{}, err
	}
	return p.open(org, agent, 0, row.Prefix, row.PasswordEnvelope)
}

// At returns the agent's repository of the given epoch, current or retired. It creates nothing:
// an epoch the agent does not have is [ErrNoRepository].
func (p Passwords) At(ctx context.Context, org, agent pgtype.UUID, epoch int32) (Repository, error) {
	if epoch == 0 {
		row, err := p.Q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: agent})
		if errors.Is(err, pgx.ErrNoRows) {
			return Repository{}, ErrNoRepository
		} else if err != nil {
			return Repository{}, err
		}
		return p.open(org, agent, 0, row.Prefix, row.PasswordEnvelope)
	}
	e, err := p.Q.GetHomeRepositoryEpoch(ctx, store.GetHomeRepositoryEpochParams{OrgID: org, AgentID: agent, RepositoryEpoch: epoch})
	if errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, ErrNoRepository
	} else if err != nil {
		return Repository{}, err
	}
	return p.open(org, agent, epoch, e.Prefix, e.PasswordEnvelope)
}

// NewEpoch starts the agent's next repository epoch, with a new random password at
// [EpochPrefix], and retires the current one: its blobs may be deleted after retain. Checkpoints
// already written stay in the retired repository. An agent without a repository has nothing to
// replace: NewEpoch returns false and its first repository is created on first use, under the
// current key. reason is [ReasonKeyLost] or [ReasonKeyCompromised].
func (p Passwords) NewEpoch(ctx context.Context, org, agent pgtype.UUID, reason string, retain time.Duration) (Repository, bool, error) {
	if p.Begin == nil {
		return Repository{}, false, errors.New("checkpoint: Passwords.NewEpoch needs Begin")
	}
	if reason != ReasonKeyLost && reason != ReasonKeyCompromised {
		return Repository{}, false, fmt.Errorf("checkpoint: unknown repository epoch reason %q", reason)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return Repository{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := p.Q.WithTx(tx)
	if _, err := q.LockHomeRepository(ctx, store.LockHomeRepositoryParams{OrgID: org, AgentID: agent}); errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, false, nil
	} else if err != nil {
		return Repository{}, false, err
	}
	var cur int32
	if e, err := q.LatestHomeRepositoryEpoch(ctx, store.LatestHomeRepositoryEpochParams{OrgID: org, AgentID: agent}); err == nil {
		cur = e.RepositoryEpoch
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, false, err
	}
	next := cur + 1
	pw, env, err := p.seal(org, agent, next)
	if err != nil {
		return Repository{}, false, err
	}
	row, err := q.InsertHomeRepositoryEpoch(ctx, store.InsertHomeRepositoryEpochParams{
		OrgID: org, AgentID: agent, RepositoryEpoch: next, Prefix: EpochPrefix(org.String(), agent.String(), next),
		PasswordEnvelope: env, Reason: reason,
	})
	if err != nil {
		return Repository{}, false, err
	}
	deadline := pgtype.Timestamptz{Time: time.Now().Add(retain), Valid: true}
	if cur == 0 {
		_, err = q.RetireHomeRepository(ctx, store.RetireHomeRepositoryParams{OrgID: org, AgentID: agent, DeleteAfter: deadline})
	} else {
		_, err = q.RetireHomeRepositoryEpoch(ctx, store.RetireHomeRepositoryEpochParams{OrgID: org, AgentID: agent, RepositoryEpoch: cur, DeleteAfter: deadline})
	}
	if err != nil {
		return Repository{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Repository{}, false, err
	}
	return Repository{Epoch: next, Prefix: row.Prefix, Password: pw}, true, nil
}

func (p Passwords) open(org, agent pgtype.UUID, epoch int32, prefix string, raw []byte) (Repository, error) {
	if raw == nil { // credential_lost_at is set: the table's CHECK pairs the two
		return Repository{}, fmt.Errorf("%w: agent %s, epoch %d", ErrRepositoryLost, agent, epoch)
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Repository{}, fmt.Errorf("the repository password of agent %s (epoch %d) is unreadable: %w", agent, epoch, err)
	}
	pw, err := p.Keys.Open(BindingAt(org, agent, epoch), env)
	if err != nil {
		return Repository{}, fmt.Errorf("opening the repository password of agent %s (epoch %d): %w", agent, epoch, err)
	}
	return Repository{Epoch: epoch, Prefix: prefix, Password: string(pw)}, nil
}

// seal makes a random password for the agent's repository of the given epoch and seals it.
func (p Passwords) seal(org, agent pgtype.UUID, epoch int32) (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	pw := base64.RawURLEncoding.EncodeToString(raw)
	env, err := p.Keys.Seal(BindingAt(org, agent, epoch), []byte(pw))
	if err != nil {
		return "", nil, err
	}
	b, err := json.Marshal(env)
	return pw, b, err
}

func (p Passwords) create(ctx context.Context, org, agent pgtype.UUID) (store.HomeRepository, error) {
	_, b, err := p.seal(org, agent, 0)
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
