// SPDX-License-Identifier: Apache-2.0

// Package wrapping builds a process's key-wrapping keys (secretstore.Wrappers) from its
// configuration. It is the one place that does: koinon-api, its "keys" commands and the reconciler
// all call New, so a KMS adapter (old issue 416, old issue 417) is wired once and no process can miss it and write a
// KEK in the clear.
package wrapping

import (
	"context"
	"errors"
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
	// ReadOnly is a second wrapping key that only unwraps (issue 688): the KMS key of before a move.
	// After a move to a new KMS key, or back to local, the KEKs it wrapped still load until the
	// key-admin Job and "keys rewrap" move them; nothing new is ever wrapped with it, in any role.
	// Its Backend "" is none. A local KEK needs no read-only key: it always opens.
	ReadOnly Key
}

// Key is one KMS wrapping key: its backend and the settings of that backend.
type Key struct {
	// Backend is BackendOpenBao or BackendAWSKMS ("" for none).
	Backend string
	// OpenBao is the transit key when Backend is BackendOpenBao.
	OpenBao openbao.Config
	// AWSKMS is the KMS key when Backend is BackendAWSKMS; its Identity is read as for the primary.
	AWSKMS awskms.Config
}

// readOnlyPrefix starts the name of each flag of the read-only key.
const readOnlyPrefix = "read-only-"

// Flags registers the configuration's flags on fs. The chart sets them on every process that holds
// the keyring (koinon-api, the reconciler) and on the key-admin Job, from one values block.
func (c *Config) Flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.Backend, "kek-backend", BackendLocal,
		"what wraps the KEKs: local (the Secret-held KEK), openbao (an OpenBao or Vault transit key) or awskms (an AWS KMS key)")
	backendFlags(fs, "", &c.OpenBao, &c.AWSKMS)
	fs.StringVar(&c.ReadOnly.Backend, readOnlyPrefix+"kek-backend", "",
		"a second wrapping key that only unwraps, the one of before a move to a new key: openbao or awskms; empty is none. "+
			"Its settings are the --"+readOnlyPrefix+"openbao-* or --"+readOnlyPrefix+"awskms-* flags. "+
			"It must have another key ID than the primary (a stored KEK names its key by backend and key ID only): "+
			"another transit mount or key name, or another AWS key ARN")
	backendFlags(fs, readOnlyPrefix, &c.ReadOnly.OpenBao, &c.ReadOnly.AWSKMS)
}

// backendFlags registers the OpenBao and AWS KMS flags of one key, each name starting with prefix.
func backendFlags(fs *pflag.FlagSet, prefix string, o *openbao.Config, a *awskms.Config) {
	which := "kek-backend"
	if prefix != "" {
		which = prefix + which
	}
	fs.StringVar(&o.Address, prefix+"openbao-address", "", "OpenBao server, https://host:port ("+which+"=openbao)")
	fs.StringVar(&o.CACertFile, prefix+"openbao-ca-file", "", "PEM bundle that verifies OpenBao's certificate; empty is the system roots")
	fs.StringVar(&o.Namespace, prefix+"openbao-namespace", "", "OpenBao namespace, if any")
	fs.StringVar(&o.TransitMount, prefix+"openbao-transit-mount", "transit", "mount path of the transit engine")
	fs.StringVar(&o.Key, prefix+"openbao-key", "", "name of the transit key that wraps the KEKs")
	fs.StringVar(&o.AuthMount, prefix+"openbao-auth-mount", "kubernetes", "mount path of the Kubernetes auth method")
	fs.StringVar(&o.Role, prefix+"openbao-role", "", "Kubernetes auth role this pod signs in as")
	fs.StringVar(&o.TokenFile, prefix+"openbao-token-file", openbao.DefaultTokenFile, "the pod's service account token")
	fs.DurationVar(&o.Timeout, prefix+"openbao-timeout", 0, "bound on one OpenBao call; 0 is the default (5s)")
	fs.StringVar(&a.Region, prefix+"awskms-region", "", "AWS region of the KMS key ("+which+"=awskms)")
	fs.StringVar(&a.KeyID, prefix+"awskms-key-id", "", "the KMS key that wraps the KEKs: key ARN (recommended), key ID, alias/<name> or alias ARN")
	fs.StringVar(&a.Endpoint, prefix+"awskms-endpoint", "", "KMS endpoint, https://host[:port] (a VPC endpoint); empty is the region's")
	fs.StringVar(&a.STSEndpoint, prefix+"awskms-sts-endpoint", "", "STS endpoint for IRSA, https://host[:port]; empty is the region's")
	fs.StringVar(&a.CACertFile, prefix+"awskms-ca-file", "", "PEM bundle that verifies the KMS and STS endpoints; empty is the system roots")
	fs.DurationVar(&a.Timeout, prefix+"awskms-timeout", 0, "bound on one AWS KMS call; 0 is the default (5s)")
}

// ErrReadOnlyKey means something tried to wrap with the read-only key: it only unwraps.
var ErrReadOnlyKey = errors.New("wrapping: the read-only wrapping key only unwraps; new KEKs are wrapped with the primary")

// readOnly is a wrapper that unwraps and probes with its key and refuses every wrap before it calls
// the key. secretstore.Wrappers already wraps with the primary only; this holds even if that changes.
type readOnly struct{ secretstore.Wrapper }

// Wrap implements secretstore.Wrapper: it always refuses.
func (r readOnly) Wrap(context.Context, []byte, []byte) ([]byte, error) {
	return nil, fmt.Errorf("wrap with %s:%s: %w", r.Backend(), r.KeyID(), ErrReadOnlyKey)
}

// adapter builds the wrapper of one KMS key. Tests replace it (export_test.go).
var adapter = func(k Key) (secretstore.Wrapper, error) {
	switch k.Backend {
	case BackendOpenBao:
		return openbao.New(k.OpenBao)
	case BackendAWSKMS:
		c := k.AWSKMS
		if c.Identity == (awskms.Identity{}) {
			var ignored []string
			c.Identity, ignored = awskms.IdentityFromEnv()
			if len(ignored) > 0 {
				slog.Warn("static AWS keys are ignored: the awskms backend signs in with the pod's identity (IRSA or EKS Pod Identity) only", "variables", ignored)
			}
		}
		return awskms.New(c)
	}
	return nil, fmt.Errorf("unknown KEK backend %q (local, openbao or awskms)", k.Backend)
}

// New returns the wrappers for cfg in role. KEKs stored before a switch to a KMS still unwrap
// (secretstore.Wrappers always opens a local one) until the key-admin Job rewraps them; KEKs stored
// under cfg.ReadOnly unwrap too, and a KEK under any other key fails to load (fail closed). boundOnly
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
		if s := openBaoSettings("", cfg.OpenBao); len(s) > 0 {
			slog.Warn("OpenBao settings are ignored: the KEK backend is local (set --kek-backend=openbao to use them)", "settings", s)
		}
		if s := awsKMSSettings("", cfg.AWSKMS); len(s) > 0 {
			slog.Warn("AWS KMS settings are ignored: the KEK backend is local (set --kek-backend=awskms to use them)", "settings", s)
		}
		primary = secretstore.Local{}
	case BackendOpenBao, BackendAWSKMS:
		w, err := adapter(Key{Backend: cfg.Backend, OpenBao: cfg.OpenBao, AWSKMS: cfg.AWSKMS})
		if err != nil {
			return secretstore.Wrappers{}, err
		}
		primary = w
	default:
		return secretstore.Wrappers{}, fmt.Errorf("unknown KEK backend %q (local, openbao or awskms)", cfg.Backend)
	}
	more, err := readOnlyKey(cfg.ReadOnly)
	if err != nil {
		return secretstore.Wrappers{}, err
	}
	if role == KeyAdmin {
		return secretstore.NewKeyAdminWrappers(boundOnly, primary, more...)
	}
	return secretstore.NewWrappers(boundOnly, primary, more...)
}

// readOnlyKey builds the read-only key of k: none, or one wrapper that refuses to wrap.
func readOnlyKey(k Key) ([]secretstore.Wrapper, error) {
	switch k.Backend {
	case "":
		if s := append(openBaoSettings(readOnlyPrefix, k.OpenBao), awsKMSSettings(readOnlyPrefix, k.AWSKMS)...); len(s) > 0 {
			slog.Warn("the read-only key's settings are ignored: no read-only key is configured (set --"+readOnlyPrefix+"kek-backend to use them)", "settings", s)
		}
		return nil, nil
	case BackendLocal:
		// A local KEK always opens; a "local" read-only key is a mistake in the configuration.
		return nil, fmt.Errorf("--%skek-backend=local: local KEKs always unwrap, so the read-only key is a KMS key (openbao or awskms) or none", readOnlyPrefix)
	case BackendOpenBao, BackendAWSKMS:
		w, err := adapter(k)
		if err != nil {
			return nil, fmt.Errorf("the read-only wrapping key: %w", err)
		}
		return []secretstore.Wrapper{readOnly{w}}, nil
	}
	return nil, fmt.Errorf("unknown read-only KEK backend %q (openbao or awskms)", k.Backend)
}

// openBaoSettings names the OpenBao settings of c (flags starting with prefix) that differ from the flags' defaults.
func openBaoSettings(prefix string, c openbao.Config) []string {
	var set []string
	add := func(name string, on bool) {
		if on {
			set = append(set, prefix+name)
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

// awsKMSSettings names the AWS KMS settings of c (flags starting with prefix) that differ from the flags' defaults.
func awsKMSSettings(prefix string, c awskms.Config) []string {
	var set []string
	add := func(name string, on bool) {
		if on {
			set = append(set, prefix+name)
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
