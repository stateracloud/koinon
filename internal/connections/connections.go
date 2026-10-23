// SPDX-License-Identifier: Apache-2.0

// Package connections holds what every user of a connection needs besides its
// Postgres row (plan item C1): the Kubernetes Secret with its secure-input
// fields (ADR 0021, 0049), loading a GitHub App from it, and the setup guide's
// built-in checks.
//
// Secret material only ever moves between the API request, this package and
// the Kubernetes API. It is never stored in Postgres, returned, logged or put
// into an error.
package connections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
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
const GitHubPrivateKeyField = "privateKey"

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

// ErrNoSecretStore is returned when Koinon runs without access to Kubernetes
// Secrets, so secure inputs cannot be stored.
var ErrNoSecretStore = errors.New("connections: no secret store configured (koinon-api needs Kubernetes access to write connection Secrets)")

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
type GitHubConfig struct {
	AppID          int64 `json:"appId"`
	InstallationID int64 `json:"installationId"`
}

// ParseGitHubConfig decodes connections.config of a GitHub connection.
func ParseGitHubConfig(raw []byte) (GitHubConfig, error) {
	var c GitHubConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("connections: decode GitHub config: %w", err)
	}
	return c, nil
}

// LoadGitHubApp reads a GitHub connection's App from its row and Secret.
func LoadGitHubApp(ctx context.Context, secrets SecretStore, c store.Connection) (github.App, error) {
	if c.Connector != connectors.GitHubID {
		return github.App{}, fmt.Errorf("connections: %s is a %s connection, not GitHub", c.ID.String(), c.Connector)
	}
	if secrets == nil {
		return github.App{}, ErrNoSecretStore
	}
	cfg, err := ParseGitHubConfig(c.Config)
	if err != nil {
		return github.App{}, err
	}
	sec, found, err := secrets.Get(ctx, c.SecretName)
	if err != nil {
		return github.App{}, err
	}
	if !found {
		return github.App{}, fmt.Errorf("connections: the Secret %s of connection %s is missing; create the connection again", c.SecretName, c.ID.String())
	}
	key, err := github.ParsePrivateKey(sec.Data[GitHubPrivateKeyField])
	if err != nil {
		return github.App{}, fmt.Errorf("connections: Secret %s: %w", c.SecretName, err)
	}
	return github.App{AppID: cfg.AppID, InstallationID: cfg.InstallationID, Key: key}, nil
}
