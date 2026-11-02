// SPDX-License-Identifier: Apache-2.0

// Package wrapping builds a process's key-wrapping keys (secretstore.Wrappers) from its
// configuration. It is the one place that does: koinon-api, its "keys" commands and the reconciler
// all call New, so a KMS adapter (#416, #417) is wired once and no process can miss it and write a
// KEK in the clear.
package wrapping

import (
	"context"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// Role is who the process is to the wrapping key.
type Role int

const (
	// Service is koinon-api (and its "keys" commands), the reconciler and agentd: decrypt-only. A
	// service wraps a new KEK only while the primary is the local Secret-held KEK.
	Service Role = iota
	// KeyAdmin is the key-admin Job (procoduck, 2026-11-02; it comes with the KMS adapters): the only
	// identity that wraps a new KEK with a KMS.
	KeyAdmin
)

// Config is the key-wrapping configuration of a process. A KMS is an option, not a mandate: the
// zero value is the local Secret-held KEK, the default and fully supported, and an install that
// keeps it has no key-admin Job and behaves exactly as before key wrapping. A KMS (OpenBao or AWS)
// is opt-in: its adapter adds its settings here (signing in with the pod's identity, never a
// credential) and its construction to New. The key-admin Job and the decrypt-only services apply
// only once a KMS primary is configured.
type Config struct{}

// New returns the wrappers for cfg in role.
func New(_ context.Context, _ Config, role Role) (secretstore.Wrappers, error) {
	if role == KeyAdmin {
		return secretstore.NewKeyAdminWrappers(secretstore.Local{})
	}
	return secretstore.NewWrappers(secretstore.Local{})
}
