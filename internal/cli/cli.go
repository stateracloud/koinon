// SPDX-License-Identifier: Apache-2.0

// Package cli holds what Koinon's binaries share on the command line: flags settable from
// KOINON_* environment variables, and the connection to the OpenShell gateway.
package cli

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/stateracloud/koinon/internal/execution/openshell"
)

// EnvPrefix starts the environment variable of every flag.
const EnvPrefix = "KOINON_"

// EnvName is the environment variable of a flag: --openshell-tls-dir is
// KOINON_OPENSHELL_TLS_DIR.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// BindEnv sets each flag of fs whose environment variable (EnvName) is set. Call it before
// fs.Parse, so that command-line flags win. getenv is os.LookupEnv when nil. The first value
// that does not parse is returned as an error naming its variable.
func BindEnv(fs *flag.FlagSet, getenv func(string) (string, bool)) error {
	if getenv == nil {
		getenv = os.LookupEnv
	}
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		key := EnvName(f.Name)
		if v, ok := getenv(key); ok {
			if err := f.Value.Set(v); err != nil && envErr == nil {
				envErr = fmt.Errorf("%s: %w", key, err)
			}
		}
	})
	return envErr
}

// OpenShell is how a binary reaches the OpenShell gateway.
type OpenShell struct {
	// Address is the gateway's host:port.
	Address string
	// TLSDir holds the client bundle OpenShell issues (tls.crt, tls.key, ca.crt). Empty
	// connects without TLS: the chart allows an empty client TLS Secret, for example for a
	// gateway behind a mesh that terminates mTLS.
	TLSDir string
	// ServerName overrides the name checked against the gateway's certificate.
	ServerName string
}

// Dial connects to the gateway. Closing the Backend closes the connection.
func (o OpenShell) Dial() (*openshell.Backend, error) {
	if o.TLSDir == "" {
		slog.Warn("connecting to the OpenShell gateway without TLS", "address", o.Address)
		return openshell.DialPlaintext(o.Address)
	}
	return openshell.Dial(openshell.Config{
		Endpoint:   o.Address,
		CertFile:   filepath.Join(o.TLSDir, "tls.crt"),
		KeyFile:    filepath.Join(o.TLSDir, "tls.key"),
		CAFile:     filepath.Join(o.TLSDir, "ca.crt"),
		ServerName: o.ServerName,
	})
}
