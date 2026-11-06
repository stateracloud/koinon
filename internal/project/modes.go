// SPDX-License-Identifier: Apache-2.0

package project

import "fmt"

// GatewayMode is the chart's gateway.mode: how agentgateway runs.
type GatewayMode string

// Gateway modes.
const (
	// GatewayStandalone runs agentgateway from a rendered local config file.
	GatewayStandalone GatewayMode = "standalone"
	// GatewayKubernetes runs agentgateway's Kubernetes controller behind a GatewayClass. It is
	// frozen: no new features, deleted with agentgateway in ADR 0097 step 3 (v0.3) (ADR 0097 decision 4, issue #274).
	GatewayKubernetes GatewayMode = "kubernetes"
)

func (m GatewayMode) String() string { return string(m) }

// Type implements pflag.Value.
func (GatewayMode) Type() string { return "gateway-mode" }

// Set implements flag.Value and accepts only the gateway modes.
func (m *GatewayMode) Set(s string) error {
	switch v := GatewayMode(s); v {
	case GatewayStandalone, GatewayKubernetes:
		*m = v
		return nil
	}
	return fmt.Errorf("gateway mode %q is neither %s nor %s", s, GatewayStandalone, GatewayKubernetes)
}

// UnmarshalText implements encoding.TextUnmarshaler, for config files.
func (m *GatewayMode) UnmarshalText(b []byte) error { return m.Set(string(b)) }

// PostgresMode is the chart's postgres.mode: where Koinon's database comes from.
type PostgresMode string

// Postgres modes.
const (
	// PostgresCNPG has the chart create a CloudNativePG Cluster.
	PostgresCNPG PostgresMode = "cnpg"
	// PostgresExternal uses a database the operator provides.
	PostgresExternal PostgresMode = "external"
)

func (m PostgresMode) String() string { return string(m) }

// Type implements pflag.Value.
func (PostgresMode) Type() string { return "postgres-mode" }

// Set implements flag.Value and accepts only the Postgres modes.
func (m *PostgresMode) Set(s string) error {
	switch v := PostgresMode(s); v {
	case PostgresCNPG, PostgresExternal:
		*m = v
		return nil
	}
	return fmt.Errorf("postgres mode %q is neither %s nor %s", s, PostgresCNPG, PostgresExternal)
}

// UnmarshalText implements encoding.TextUnmarshaler, for config files.
func (m *PostgresMode) UnmarshalText(b []byte) error { return m.Set(string(b)) }
