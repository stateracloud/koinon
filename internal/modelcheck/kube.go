// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KeyDataKey is the data key of the Secret that holds the org's check key.
const KeyDataKey = "api_key"

// KeySecretName is the Secret that holds an org's echo-check key: a gateway key of the org that may
// request the org's models, kept by koinon-reconciler for koinon-api to read.
func KeySecretName(org pgtype.UUID) string {
	sum := sha256.Sum256([]byte("model-check\x00" + org.String()))
	return "koinon-model-check-" + hex.EncodeToString(sum[:8])
}

// KubeKeys reads the check keys from Secrets in one namespace.
type KubeKeys struct {
	Client    kubernetes.Interface
	Namespace string
}

// Key implements KeySource.
func (k KubeKeys) Key(ctx context.Context, org pgtype.UUID) (string, error) {
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, KeySecretName(org), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the org's echo-check key: %w", err)
	}
	if len(s.Data[KeyDataKey]) == 0 {
		return "", errors.New("the org's echo-check key is empty")
	}
	return string(s.Data[KeyDataKey]), nil
}
