// SPDX-License-Identifier: Apache-2.0

// Package ensuresecrets creates the Secrets the chart cannot render: ones with a generated
// password. A password in a template is new on every render, and a render has no cluster to read
// the live Secret from (helm template, Argo CD, Flux), so the chart's hook Job calls Ensure instead.
// An existing Secret is never changed.
package ensuresecrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PasswordKey is the data key that holds the password, and the one a kept Secret must have.
const PasswordKey = "password"

// UsernameKey is the data key that holds the login name.
const UsernameKey = "username"

// KeyKey is the data key that holds a generated AES-256 key (Spec.Kind KindAES256Key).
const KeyKey = "key"

// TokenKey is the data key that holds a generated bearer token (Spec.Kind KindBearerToken).
const TokenKey = "token"

// KindBearerToken is Spec.Kind of a Secret that holds a random 256-bit bearer token as 64 hex
// characters under TokenKey (the internal OTLP listener's two, ADR 0074). An operator rotates it by
// moving the old value to a "previous" key and setting a new "token" (internal/otlpauth).
const KindBearerToken = "bearer-token"

// KindAES256Key is Spec.Kind of a Secret that holds a random 256-bit key as 64 hex characters under
// KeyKey, the form agentgateway's SESSION_KEY takes, instead of a username and a password.
const KindAES256Key = "aes256-key"

// PasswordLength is the length of a generated password.
const PasswordLength = 32

var hexKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// bearerRE is the characters of a header-safe bearer token.
var bearerRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Spec is one Secret to ensure.
type Spec struct {
	Name string `json:"name"`
	// Type is the Secret type, for example kubernetes.io/basic-auth or Opaque.
	Type corev1.SecretType `json:"type"`
	// Username is written under the key "username".
	Username string `json:"username"`
	// Kind is empty for a username and a password, or KindAES256Key for a hex key.
	Kind   string            `json:"kind,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Result is what Ensure did with one Secret.
type Result struct {
	Name    string
	Created bool
}

// Ensure makes sure every Spec exists in namespace. A missing Secret is created with a new random
// password; an existing one is left exactly as it is, but must have a password, and a
// kubernetes.io/basic-auth one (a database role's) must also have that type and the spec's username,
// which is the role CloudNativePG logs in as. A Create that loses a
// race counts as success. It stops at the first error. Passwords are never logged: progress goes to
// out as names and "created" or "kept".
func Ensure(ctx context.Context, client kubernetes.Interface, namespace string, specs []Spec, out io.Writer) ([]Result, error) {
	secrets := client.CoreV1().Secrets(namespace)
	results := make([]Result, 0, len(specs))
	for _, sp := range specs {
		if sp.Name == "" {
			return results, errors.New("a Secret spec has no name")
		}
		if sp.Kind != "" && sp.Kind != KindAES256Key && sp.Kind != KindBearerToken {
			return results, fmt.Errorf("the Secret spec %s has an unknown kind %q", sp.Name, sp.Kind)
		}
		existing, err := secrets.Get(ctx, sp.Name, metav1.GetOptions{})
		switch {
		case err == nil && sp.Kind == KindAES256Key:
			if k := string(existing.Data[KeyKey]); !hexKey.MatchString(k) {
				return results, fmt.Errorf("the Secret %s/%s exists but its %q key is not 64 hex characters; fix or delete it (it is not changed here)", namespace, sp.Name, KeyKey)
			}
			results = append(results, Result{Name: sp.Name})
			_, _ = fmt.Fprintf(out, "secret %s: kept\n", sp.Name)
			continue
		case err == nil && sp.Kind == KindBearerToken:
			// Rotated by hand, so any header-safe token of enough length is kept: the format is
			// not the generator's.
			if t := strings.TrimSpace(string(existing.Data[TokenKey])); len(t) < 32 || !bearerRE.MatchString(t) {
				return results, fmt.Errorf("the Secret %s/%s exists but its %q key is not a token of at least 32 characters; fix or delete it (it is not changed here)", namespace, sp.Name, TokenKey)
			}
			// "previous" is optional (a rotation in progress); when present and not blank it must be valid
			// too, or the listener would ignore it and the old token would stop working unnoticed.
			if p := strings.TrimSpace(string(existing.Data["previous"])); p != "" && (len(p) < 32 || !bearerRE.MatchString(p)) {
				return results, fmt.Errorf("the Secret %s/%s exists but its \"previous\" key is not a token of at least 32 characters; fix it or remove the key (it is not changed here)", namespace, sp.Name)
			}
			results = append(results, Result{Name: sp.Name})
			_, _ = fmt.Fprintf(out, "secret %s: kept\n", sp.Name)
			continue
		case err == nil:
			if len(existing.Data[PasswordKey]) == 0 && existing.StringData[PasswordKey] == "" {
				return results, fmt.Errorf("the Secret %s/%s exists but has no %q key; fix or delete it (it is not changed here)", namespace, sp.Name, PasswordKey)
			}
			if sp.Type == corev1.SecretTypeBasicAuth {
				if existing.Type != corev1.SecretTypeBasicAuth {
					return results, fmt.Errorf("the Secret %s/%s exists with type %q, want %q; fix or delete it (it is not changed here)", namespace, sp.Name, existing.Type, corev1.SecretTypeBasicAuth)
				}
				if got := string(existing.Data[UsernameKey]); got != sp.Username {
					return results, fmt.Errorf("the Secret %s/%s exists with username %q, want %q; fix or delete it (it is not changed here)", namespace, sp.Name, got, sp.Username)
				}
			}
			results = append(results, Result{Name: sp.Name})
			_, _ = fmt.Fprintf(out, "secret %s: kept\n", sp.Name)
			continue
		case !apierrors.IsNotFound(err):
			return results, fmt.Errorf("read the Secret %s/%s: %w", namespace, sp.Name, err)
		}
		data := map[string][]byte{}
		if sp.Kind == KindAES256Key || sp.Kind == KindBearerToken {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return results, fmt.Errorf("generate a key: %w", err)
			}
			if sp.Kind == KindBearerToken {
				data[TokenKey] = []byte(hex.EncodeToString(b))
			} else {
				data[KeyKey] = []byte(hex.EncodeToString(b))
			}
		} else {
			password, err := NewPassword()
			if err != nil {
				return results, err
			}
			data[UsernameKey] = []byte(sp.Username)
			data[PasswordKey] = []byte(password)
		}
		typ := sp.Type
		if typ == "" {
			typ = corev1.SecretTypeOpaque
		}
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sp.Name, Namespace: namespace, Labels: sp.Labels},
			Type:       typ,
			Data:       data,
		}, metav1.CreateOptions{FieldManager: "koinon-api"})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return results, fmt.Errorf("create the Secret %s/%s: %w", namespace, sp.Name, err)
		}
		created := err == nil
		results = append(results, Result{Name: sp.Name, Created: created})
		if created {
			_, _ = fmt.Fprintf(out, "secret %s: created\n", sp.Name)
		} else {
			_, _ = fmt.Fprintf(out, "secret %s: kept (created concurrently)\n", sp.Name)
		}
	}
	return results, nil
}

// NewPassword returns PasswordLength random alphanumeric characters from crypto/rand.
func NewPassword() (string, error) {
	max := big.NewInt(int64(len(alphabet)))
	b := make([]byte, PasswordLength)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate a password: %w", err)
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}
