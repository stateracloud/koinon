// SPDX-License-Identifier: Apache-2.0

// Package wrapping builds a process's key-wrapping keys (secretstore.Wrappers) from its
// configuration. It is the one place that does: koinon-api, its "keys" commands and the reconciler
// all call New, so a KMS adapter (old issue 416, old issue 417) is wired once and no process can miss it and write a
// KEK in the clear.
package wrapping

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/awskms"
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
	// BackendOpenBao is an OpenBao (or Vault) transit key (old issue 416).
	BackendOpenBao = openbao.Backend
	// BackendAWSKMS is an AWS KMS symmetric key (old issue 417).
	BackendAWSKMS = awskms.Backend
)

// Config is the key-wrapping configuration of a process. A KMS is an option, not a mandate: the
// zero value is the local Secret-held KEK, the default and fully supported, and an install that
// keeps it has no key-admin Job and behaves exactly as before key wrapping. A KMS (OpenBao or AWS)
// is opt-in: its adapter adds its settings here (signing in with the pod's identity, never a
// credential) and its construction to New. The key-admin Job and the decrypt-only services apply
// only once a KMS primary is configured.
type Config struct {
	// Backend is the primary: "" or BackendLocal, BackendOpenBao or BackendAWSKMS.
	Backend string
	// OpenBao is the transit key when Backend is BackendOpenBao.
	OpenBao openbao.Config
	// AWSKMS is the KMS key when Backend is BackendAWSKMS. Its Identity is not a flag: New reads it
	// from the pod's environment (IRSA or EKS Pod Identity) when it is unset.
	AWSKMS awskms.Config
}

// Flags registers the configuration's flags on fs. The chart sets them on every process that holds
// the keyring (koinon-api, the reconciler) and on the key-admin Job, from one values block.
func (c *Config) Flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.Backend, "kek-backend", BackendLocal,
		"what wraps the KEKs: local (the Secret-held KEK), openbao (an OpenBao or Vault transit key) or awskms (an AWS KMS key)")
	fs.StringVar(&c.OpenBao.Address, "openbao-address", "", "OpenBao server, https://host:port (kek-backend=openbao)")
	fs.StringVar(&c.OpenBao.CACertFile, "openbao-ca-file", "", "PEM bundle that verifies OpenBao's certificate; empty is the system roots")
	fs.StringVar(&c.OpenBao.Namespace, "openbao-namespace", "", "OpenBao namespace, if any")
	fs.StringVar(&c.OpenBao.TransitMount, "openbao-transit-mount", "transit", "mount path of the transit engine")
	fs.StringVar(&c.OpenBao.Key, "openbao-key", "", "name of the transit key that wraps the KEKs")
	fs.StringVar(&c.OpenBao.AuthMount, "openbao-auth-mount", "kubernetes", "mount path of the Kubernetes auth method")
	fs.StringVar(&c.OpenBao.Role, "openbao-role", "", "Kubernetes auth role this pod signs in as")
	fs.StringVar(&c.OpenBao.TokenFile, "openbao-token-file", openbao.DefaultTokenFile, "the pod's service account token")
	fs.DurationVar(&c.OpenBao.Timeout, "openbao-timeout", 0, "bound on one OpenBao call; 0 is the default (5s)")
	fs.StringVar(&c.AWSKMS.Region, "awskms-region", "", "AWS region of the KMS key (kek-backend=awskms)")
	fs.StringVar(&c.AWSKMS.KeyID, "awskms-key-id", "", "the KMS key that wraps the KEKs: key ARN (recommended), key ID, alias/<name> or alias ARN")
	fs.StringVar(&c.AWSKMS.Endpoint, "awskms-endpoint", "", "KMS endpoint, https://host[:port] (a VPC endpoint); empty is the region's")
	fs.StringVar(&c.AWSKMS.STSEndpoint, "awskms-sts-endpoint", "", "STS endpoint for IRSA, https://host[:port]; empty is the region's")
	fs.StringVar(&c.AWSKMS.CACertFile, "awskms-ca-file", "", "PEM bundle that verifies the KMS and STS endpoints; empty is the system roots")
	fs.DurationVar(&c.AWSKMS.Timeout, "awskms-timeout", 0, "bound on one AWS KMS call; 0 is the default (5s)")
}

// New returns the wrappers for cfg in role. KEKs stored before a switch to a KMS still unwrap
// (secretstore.Wrappers always opens a local one) until the key-admin Job rewraps them. boundOnly
// reads the install's bound-only marker (old issue 615) and is required: see [secretstore.NewWrappers].
func New(_ context.Context, cfg Config, role Role, boundOnly secretstore.BoundOnlySource) (secretstore.Wrappers, error) {
	if boundOnly == nil {
		return secretstore.Wrappers{}, secretstore.ErrNoBoundOnlySource
	}
	var primary secretstore.Wrapper
	switch cfg.Backend {
	case "", BackendLocal:
		// Likely a half-done switch to a KMS (the backend flag forgotten): say so, since the process
		// would otherwise wrap new KEKs locally without a word.
		if s := openBaoSettings(cfg.OpenBao); len(s) > 0 {
			slog.Warn("OpenBao settings are ignored: the KEK backend is local (set --kek-backend=openbao to use them)", "settings", s)
		}
		if s := awsKMSSettings(cfg.AWSKMS); len(s) > 0 {
			slog.Warn("AWS KMS settings are ignored: the KEK backend is local (set --kek-backend=awskms to use them)", "settings", s)
		}
		primary = secretstore.Local{}
	case BackendOpenBao:
		w, err := openbao.New(cfg.OpenBao)
		if err != nil {
			return secretstore.Wrappers{}, err
		}
		primary = w
	case BackendAWSKMS:
		c := cfg.AWSKMS
		if c.Identity == (awskms.Identity{}) {
			var ignored []string
			c.Identity, ignored = awskms.IdentityFromEnv()
			if len(ignored) > 0 {
				slog.Warn("static AWS keys are ignored: the awskms backend signs in with the pod's identity (IRSA or EKS Pod Identity) only", "variables", ignored)
			}
		}
		w, err := awskms.New(c)
		if err != nil {
			return secretstore.Wrappers{}, err
		}
		primary = w
	default:
		return secretstore.Wrappers{}, fmt.Errorf("unknown KEK backend %q (local, openbao or awskms)", cfg.Backend)
	}
	if role == KeyAdmin {
		return secretstore.NewKeyAdminWrappers(boundOnly, primary)
	}
	return secretstore.NewWrappers(boundOnly, primary)
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

// awsKMSSettings names the AWS KMS settings of c that differ from the flags' defaults.
func awsKMSSettings(c awskms.Config) []string {
	var set []string
	add := func(name string, on bool) {
		if on {
			set = append(set, name)
		}
	}
	add("awskms-region", c.Region != "")
	add("awskms-key-id", c.KeyID != "")
	add("awskms-endpoint", c.Endpoint != "")
	add("awskms-sts-endpoint", c.STSEndpoint != "")
	add("awskms-ca-file", c.CACertFile != "")
	add("awskms-timeout", c.Timeout != 0)
	return set
}
