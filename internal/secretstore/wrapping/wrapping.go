// SPDX-License-Identifier: Apache-2.0

// Package wrapping builds a process's key-wrapping keys (secretstore.Wrappers) from its
// configuration. It is the one place that does: koinon-api, its "keys" commands and the reconciler
// all call New, so a KMS adapter (#416, #417) is wired once and no process can miss it and write a
// KEK in the clear.
package wrapping

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/openbao"
)

// Role is who the process is to the wrapping key.
type Role int

const (
	// Service is koinon-api (and its "keys" commands), and the reconciler: decrypt-only (agentd holds no keyring). A
	// service wraps a new KEK only while the primary is the local Secret-held KEK.
	Service Role = iota
	// KeyAdmin is the key-admin Job (procoduck, 2026-11-02; it comes with the KMS adapters): the only
	// identity that wraps a new KEK with a KMS.
	KeyAdmin
)

// The backends a process can be configured with.
const (
	// BackendLocal is the local Secret-held KEK (the default).
	BackendLocal = secretstore.BackendLocal
	// BackendOpenBao is an OpenBao (or Vault) transit key (#416).
	BackendOpenBao = openbao.Backend
)

// Config is the key-wrapping configuration of a process. A KMS is an option, not a mandate: the
// zero value is the local Secret-held KEK, the default and fully supported, and an install that
// keeps it has no key-admin Job and behaves exactly as before key wrapping. A KMS (OpenBao or AWS)
// is opt-in: its adapter adds its settings here (signing in with the pod's identity, never a
// credential) and its construction to New. The key-admin Job and the decrypt-only services apply
// only once a KMS primary is configured.
type Config struct {
	// Backend is the primary: "" or BackendLocal, or BackendOpenBao.
	Backend string
	// OpenBao is the transit key when Backend is BackendOpenBao.
	OpenBao openbao.Config
}

// Flags registers the configuration's flags on fs. The chart sets them on every process that holds
// the keyring (koinon-api, the reconciler) and on the key-admin Job, from one values block.
func (c *Config) Flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.Backend, "kek-backend", BackendLocal,
		"what wraps the KEKs: local (the Secret-held KEK) or openbao (an OpenBao or Vault transit key)")
	fs.StringVar(&c.OpenBao.Address, "openbao-address", "", "OpenBao server, https://host:port (kek-backend=openbao)")
	fs.StringVar(&c.OpenBao.CACertFile, "openbao-ca-file", "", "PEM bundle that verifies OpenBao's certificate; empty is the system roots")
	fs.StringVar(&c.OpenBao.Namespace, "openbao-namespace", "", "OpenBao namespace, if any")
	fs.StringVar(&c.OpenBao.TransitMount, "openbao-transit-mount", "transit", "mount path of the transit engine")
	fs.StringVar(&c.OpenBao.Key, "openbao-key", "", "name of the transit key that wraps the KEKs")
	fs.StringVar(&c.OpenBao.AuthMount, "openbao-auth-mount", "kubernetes", "mount path of the Kubernetes auth method")
	fs.StringVar(&c.OpenBao.Role, "openbao-role", "", "Kubernetes auth role this pod signs in as")
	fs.StringVar(&c.OpenBao.TokenFile, "openbao-token-file", openbao.DefaultTokenFile, "the pod's service account token")
	fs.DurationVar(&c.OpenBao.Timeout, "openbao-timeout", 0, "bound on one OpenBao call; 0 is the default (5s)")
}

// New returns the wrappers for cfg in role. KEKs stored before a switch to a KMS still unwrap
// (secretstore.Wrappers always opens a local one) until the key-admin Job rewraps them.
func New(_ context.Context, cfg Config, role Role) (secretstore.Wrappers, error) {
	var primary secretstore.Wrapper
	switch cfg.Backend {
	case "", BackendLocal:
		if s := openBaoSettings(cfg.OpenBao); len(s) > 0 {
			// Likely a half-done switch to OpenBao (the backend flag forgotten): say so, since the
			// process would otherwise wrap new KEKs locally without a word.
			slog.Warn("OpenBao settings are ignored: the KEK backend is local (set --kek-backend=openbao to use them)", "settings", s)
		}
		primary = secretstore.Local{}
	case BackendOpenBao:
		w, err := openbao.New(cfg.OpenBao)
		if err != nil {
			return secretstore.Wrappers{}, err
		}
		primary = w
	default:
		return secretstore.Wrappers{}, fmt.Errorf("unknown KEK backend %q (local or openbao)", cfg.Backend)
	}
	if role == KeyAdmin {
		return secretstore.NewKeyAdminWrappers(primary)
	}
	return secretstore.NewWrappers(primary)
}

// openBaoSettings names the OpenBao settings of c that differ from the flags' defaults.
func openBaoSettings(c openbao.Config) []string {
	var set []string
	add := func(name string, on bool) {
		if on {
			set = append(set, name)
		}
	}
	add("openbao-address", c.Address != "")
	add("openbao-ca-file", c.CACertFile != "")
	add("openbao-namespace", c.Namespace != "")
	add("openbao-transit-mount", c.TransitMount != "" && c.TransitMount != "transit")
	add("openbao-key", c.Key != "")
	add("openbao-auth-mount", c.AuthMount != "" && c.AuthMount != "kubernetes")
	add("openbao-role", c.Role != "")
	add("openbao-token-file", c.TokenFile != "" && c.TokenFile != openbao.DefaultTokenFile)
	add("openbao-timeout", c.Timeout != 0)
	return set
}
