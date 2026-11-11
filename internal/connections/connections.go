// SPDX-License-Identifier: Apache-2.0

// Package connections holds what every user of a connection needs besides its
// Postgres row (plan item C1): its secret fields, stored as an envelope in
// connections.secret_envelope (ADR 0056, plan item PV1; before that they lived
// in a Kubernetes Secret per connection, ADR 0021, 0049), loading a GitHub App
// from them, and the setup guide's built-in checks.
//
// Secret material moves between the API request (or a declarative Secret),
// this package and the secret store. It is never returned, logged or put into
// an error.
package connections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5/pgtype"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Labels and annotations on connection Secrets. Values are IDs, never names.
var (
	LabelManagedBy        = project.Domain + "/managed-by"
	LabelOrg              = project.Domain + "/org-id"
	LabelConnection       = project.Domain + "/connection-id"
	AnnotationConnector   = project.Domain + "/connector"
	AnnotationFingerprint = project.Domain + "/fingerprint"
	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "koinon-api"
)

// GitHubPrivateKeyField is the setup-guide field (and Secret data key) of the
// GitHub App private key.
const GitHubPrivateKeyField = github.PrivateKeyField

// Opener decrypts envelopes; *secretstore.Keyring is one.
type Opener interface {
	Open(b secretstore.Binding, e secretstore.Envelope) ([]byte, error)
}

// Sealer encrypts values; *secretstore.Keyring is one.
type Sealer interface {
	Seal(b secretstore.Binding, plaintext []byte) (secretstore.Envelope, error)
}

// ErrNoKeyring is returned when Koinon runs without key-encryption keys, so secret fields
// cannot be stored or read.
var ErrNoKeyring = errors.New("connections: no key-encryption keys configured (mount the KEK Secret into koinon-api)")

// Table and column of the envelope, as bound into its AAD.
const (
	Table  = "connections"
	Column = "secret_envelope"
)

// BindingFor is the AAD binding of a connection's secret fields.
func BindingFor(org, id pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: Table, RowID: id.String(), Column: Column}
}

// SealFields encrypts a connection's secret fields (setup-guide field ID to value) for its
// row. Fields must never be logged.
func SealFields(sealer Sealer, org, id pgtype.UUID, fields map[string][]byte) ([]byte, error) {
	if sealer == nil {
		return nil, ErrNoKeyring
	}
	plain, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	env, err := sealer.Seal(BindingFor(org, id), plain)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// OpenFields decrypts the secret fields of c.
func OpenFields(opener Opener, c store.Connection) (map[string][]byte, error) {
	if opener == nil {
		return nil, ErrNoKeyring
	}
	if len(c.SecretEnvelope) == 0 {
		return nil, fmt.Errorf("connections: connection %s has no stored secret fields yet", c.ID.String())
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(c.SecretEnvelope, &env); err != nil {
		return nil, fmt.Errorf("connections: connection %s: unreadable envelope", c.ID.String())
	}
	plain, err := opener.Open(BindingFor(c.OrgID, c.ID), env)
	if err != nil {
		return nil, fmt.Errorf("connections: connection %s: %w", c.ID.String(), err)
	}
	defer clear(plain)
	var fields map[string][]byte
	if err := json.Unmarshal(plain, &fields); err != nil {
		return nil, fmt.Errorf("connections: connection %s: unreadable secret fields", c.ID.String())
	}
	return fields, nil
}

// Legacy Kubernetes Secrets. Before PV1 a connection's secret fields lived in a Secret named
// by connections.secret_name; koinon-api moves them into the store at startup (Migrate).

// Secret is a connection's secret fields as stored in a Kubernetes Secret.
// Data must never be logged; Secret's String hides it.
type Secret struct {
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	Data        map[string][]byte
}

// String implements fmt.Stringer without the data.
func (s Secret) String() string { return "connection secret " + s.Name + " ([redacted])" }

// GoString implements fmt.GoStringer without the data.
func (s Secret) GoString() string { return s.String() }

// SecretStore reads and writes connection Secrets by name. It never lists:
// names are recorded in Postgres.
type SecretStore interface {
	// Create creates the Secret; it fails when one with the name exists.
	Create(ctx context.Context, s Secret) error
	// Get returns the Secret; found is false when it does not exist.
	Get(ctx context.Context, name string) (s Secret, found bool, err error)
	// Delete removes the Secret; a missing Secret is not an error.
	Delete(ctx context.Context, name string) error
}

// SecretName derives a connection's Secret name from its org and ID.
func SecretName(org, id [16]byte) string {
	h := sha256.New()
	h.Write(org[:])
	h.Write(id[:])
	return "koinon-conn-" + hex.EncodeToString(h.Sum(nil))[:20]
}

// KubeSecrets stores Secrets in one namespace (Koinon's own).
type KubeSecrets struct {
	Client    kubernetes.Interface
	Namespace string
}

var _ SecretStore = KubeSecrets{}

// Create implements SecretStore.
func (k KubeSecrets) Create(ctx context.Context, s Secret) error {
	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name, Namespace: k.Namespace,
			Labels: maps.Clone(s.Labels), Annotations: maps.Clone(s.Annotations),
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: ptr(true),
		Data:      s.Data,
	}
	if _, err := k.Client.CoreV1().Secrets(k.Namespace).Create(ctx, obj, metav1.CreateOptions{FieldManager: ManagedByValue}); err != nil {
		// The API server's errors name the object, not its data.
		return fmt.Errorf("create secret %s/%s: %w", k.Namespace, s.Name, err)
	}
	return nil
}

// Get implements SecretStore.
func (k KubeSecrets) Get(ctx context.Context, name string) (Secret, bool, error) {
	obj, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Secret{}, false, nil
	}
	if err != nil {
		return Secret{}, false, fmt.Errorf("get secret %s/%s: %w", k.Namespace, name, err)
	}
	return Secret{Name: name, Labels: obj.Labels, Annotations: obj.Annotations, Data: obj.Data}, true, nil
}

// Delete implements SecretStore.
func (k KubeSecrets) Delete(ctx context.Context, name string) error {
	err := k.Client.CoreV1().Secrets(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete secret %s/%s: %w", k.Namespace, name, err)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// GitHubConfig is the non-secret part of a GitHub connection, stored in
// connections.config under the setup guide's field IDs.
type GitHubConfig = github.Config

// ParseGitHubConfig decodes connections.config of a GitHub connection.
func ParseGitHubConfig(raw []byte) (GitHubConfig, error) { return github.ParseConfig(raw) }

// ErrRemoved means the connection was declared by a Secret that has been deleted.
var ErrRemoved = errors.New("connections: the connection was removed")

// ErrCredentialLost means the connection's stored secret fields were lost with a key (ADR 0085
// decision 13); they must be entered again.
var ErrCredentialLost = errors.New("connections: the connection's stored credential was lost")

// LostRemedy says how a connection's lost secret comes back: entered again on the Connections page
// for a connection entered there, stored again from its Kubernetes Secret for a declared one.
func LostRemedy(c store.Connection) string {
	if c.Source == store.ConfigSourceSecret {
		return fmt.Sprintf("the Kubernetes Secret %s that declares the connection %q stores it again at the next sync", c.ManagedBySecret.String, c.DisplayName)
	}
	return fmt.Sprintf("enter the secret of the connection %q again on the Connections page", c.DisplayName)
}

// Load opens a connection's stored secret fields for its code-host provider.
func Load(_ context.Context, opener Opener, c store.Connection) (codehost.Connection, error) {
	if c.RemovedAt.Valid {
		return codehost.Connection{}, fmt.Errorf("%w: the Kubernetes Secret %s that declared %q was deleted; restore the Secret, or bind the agent to another connection",
			ErrRemoved, c.ManagedBySecret.String, c.DisplayName)
	}
	if c.AdminDisabledAt.Valid {
		// The outward switch (ADR 0110 decision 5): every caller that dials the code host loads the connection
		// here, so a disabled connection is not called, tested or synced.
		return codehost.Connection{}, fmt.Errorf("connection %q: %w", c.DisplayName, outward.ErrDisabled)
	}
	if c.CredentialLostAt.Valid {
		return codehost.Connection{}, fmt.Errorf("%w: %s", ErrCredentialLost, LostRemedy(c))
	}
	fields, err := OpenFields(opener, c)
	if err != nil {
		return codehost.Connection{}, err
	}
	return codehost.Connection{ID: c.ID.String(), DisplayName: c.DisplayName, Connector: c.Connector, Config: c.Config, Fields: fields}, nil
}

// LoadGitHubApp reads a GitHub connection's App from its row and stored secret fields.
func LoadGitHubApp(ctx context.Context, opener Opener, c store.Connection) (github.App, error) {
	conn, err := Load(ctx, opener, c)
	if err != nil {
		return github.App{}, err
	}
	return github.AppOf(conn)
}
