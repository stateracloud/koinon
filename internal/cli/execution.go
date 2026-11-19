// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/harness"
)

// Execution drivers, the values of execution.driver (ADR 0119). OpenShell stays the default until
// the epic flips it (#1082).
const (
	DriverOpenShell = "openshell"
	DriverSubstrate = "substrate"
)

// Substrate is how a binary reaches Agent Substrate: ateapi for the control calls and the atenet
// router for the guest calls (see [substrate.Endpoint]).
type Substrate struct {
	ControlAddress  string
	RouterAddress   string
	CAFile          string
	ServerName      string
	BearerTokenFile string
	// RuntimeClassTiers maps a runtime class to its isolation tier, as "class=tier" entries (ADR 0075;
	// tiers are vm, hardened and dev). The driver puts the launcher policy of the tier in front of
	// every command, so a sandbox whose class is not here cannot run one.
	RuntimeClassTiers []string
	// DefaultTier is the tier of a sandbox that names no runtime class.
	DefaultTier string
	// AllowUnhardened acknowledges that the substrate path does not yet run the harness behind the
	// guest launcher or mount the root read-only on the gVisor tiers (issue #1088 slices b and c).
	// Validate refuses the substrate driver without it, so no install gets an unhardened guest by
	// accident. Exit condition: both slices have merged; then the flag and chart value are removed.
	AllowUnhardened bool
}

// DriverConfig is the substrate driver's configuration for these settings.
func (s Substrate) DriverConfig(logger *slog.Logger) (substrate.Config, error) {
	return s.driverConfig(logger)
}

func (s Substrate) driverConfig(logger *slog.Logger) (substrate.Config, error) {
	cfg := substrate.Config{Logger: logger}
	if s.DefaultTier != "" {
		t, err := parseTier(s.DefaultTier)
		if err != nil {
			return cfg, fmt.Errorf("--substrate-default-tier: %w", err)
		}
		cfg.DefaultTier = t
	}
	for _, entry := range s.RuntimeClassTiers {
		class, tier, ok := strings.Cut(entry, "=")
		class = strings.TrimSpace(class)
		if !ok || class == "" {
			return cfg, fmt.Errorf("--substrate-runtime-class-tiers: %q is not class=tier", entry)
		}
		t, err := parseTier(strings.TrimSpace(tier))
		if err != nil {
			return cfg, fmt.Errorf("--substrate-runtime-class-tiers: class %q: %w", class, err)
		}
		if _, dup := cfg.RuntimeClassTiers[class]; dup {
			return cfg, fmt.Errorf("--substrate-runtime-class-tiers: class %q is listed twice", class)
		}
		if cfg.RuntimeClassTiers == nil {
			cfg.RuntimeClassTiers = map[string]harness.Tier{}
		}
		cfg.RuntimeClassTiers[class] = t
	}
	return cfg, nil
}

func parseTier(v string) (harness.Tier, error) {
	t := harness.Tier(v)
	if _, ok := (harness.Descriptor{}).LauncherPrefix(t); !ok {
		return "", fmt.Errorf("tier %q: want %s, %s or %s", v, harness.TierVM, harness.TierHardened, harness.TierDev)
	}
	return t, nil
}

// Execution selects the execution driver a binary uses and says how to reach it.
type Execution struct {
	// Driver is DriverOpenShell (also the empty value) or DriverSubstrate.
	Driver    string
	OpenShell OpenShell
	Substrate Substrate
	// Owner is the install mark. OpenShell labels its objects with it; Substrate has no object to
	// label (an atespace and its actors belong to the install that made the template names).
	Owner string
}

// Flags registers the flags that select the driver and reach Substrate. The OpenShell flags stay
// with each binary, which has its own names for them.
func (e *Execution) Flags(fs *pflag.FlagSet) {
	fs.StringVar(&e.Driver, "execution-driver", DriverOpenShell, "execution.driver: openshell or substrate (ADR 0119)")
	fs.StringVar(&e.Substrate.ControlAddress, "substrate-control-address", "", "Agent Substrate's control API (ateapi) host:port, with --execution-driver=substrate")
	fs.StringVar(&e.Substrate.RouterAddress, "substrate-router-address", "", "Agent Substrate's atenet router host:port, with --execution-driver=substrate")
	fs.StringVar(&e.Substrate.CAFile, "substrate-ca-file", "", "CA file that signed ateapi's certificate; empty uses the system roots")
	fs.StringVar(&e.Substrate.ServerName, "substrate-server-name", "", "name checked against ateapi's certificate")
	fs.BoolVar(&e.Substrate.AllowUnhardened, "substrate-allow-unhardened", false, "acknowledge that the substrate path runs the harness without the guest launcher and a read-only root until issue #1088 slices b and c land; required with --execution-driver=substrate")
	fs.StringSliceVar(&e.Substrate.RuntimeClassTiers, "substrate-runtime-class-tiers", nil, "class=tier entries (tier: vm, hardened or dev) that give each runtime class its isolation tier, with --execution-driver=substrate")
	fs.StringVar(&e.Substrate.DefaultTier, "substrate-default-tier", "", "isolation tier (vm, hardened or dev) of a sandbox with no runtime class, with --execution-driver=substrate")
	fs.StringVar(&e.Substrate.BearerTokenFile, "substrate-token-file", "", "projected ServiceAccount token (ateapi audience) sent as the bearer token; read again on every call")
}

// Validate checks the driver name and what the chosen driver needs. openshellAddress is the
// binary's OpenShell address setting, which only the openshell driver needs.
func (e *Execution) Validate(openshellAddress string) error {
	switch e.Driver {
	case "", DriverOpenShell:
		if openshellAddress == "" {
			return errors.New("--openshell-address is required with --execution-driver=openshell")
		}
	case DriverSubstrate:
		if e.Substrate.ControlAddress == "" || e.Substrate.RouterAddress == "" {
			return errors.New("--substrate-control-address and --substrate-router-address are required with --execution-driver=substrate")
		}
		cfg, err := e.Substrate.driverConfig(nil)
		if err != nil {
			return err
		}
		if !e.Substrate.AllowUnhardened {
			return errors.New("--execution-driver=substrate refuses to start without --substrate-allow-unhardened: the gVisor tiers have no read-only root and the runtime class to tier map has no flag yet (issue #1088 slice c)")
		}
		if cfg.DefaultTier == "" && len(cfg.RuntimeClassTiers) == 0 {
			return errors.New("--substrate-default-tier or --substrate-runtime-class-tiers is required with --execution-driver=substrate: without a tier no guest process can start")
		}
	default:
		return fmt.Errorf("--execution-driver %q: want %s or %s", e.Driver, DriverOpenShell, DriverSubstrate)
	}
	return nil
}

// IsSubstrate reports whether the substrate driver is selected.
func (e Execution) IsSubstrate() bool { return e.Driver == DriverSubstrate }

// Connection is a dialed execution driver.
type Connection struct {
	// Backend is the driver as the reconciler takes it. The substrate driver has no Egress half and
	// is wrapped by [execution.WithoutEgress].
	Backend execution.Backend
	// Collectors are the driver's own metrics, for the binary's registry (ADR 0023: no IDs in labels).
	Collectors []prometheus.Collector
	closer     io.Closer
}

// Close releases the driver's connections.
func (c *Connection) Close() error { return c.closer.Close() }

// Dial connects to the selected driver.
func (e Execution) Dial() (*Connection, error) {
	if e.IsSubstrate() {
		cfg, err := e.Substrate.driverConfig(slog.Default())
		if err != nil {
			return nil, err
		}
		d, err := substrate.Dial(substrate.Endpoint{
			ControlAddr: e.Substrate.ControlAddress, RouterAddr: e.Substrate.RouterAddress,
			CAFile: e.Substrate.CAFile, ServerName: e.Substrate.ServerName, BearerTokenFile: e.Substrate.BearerTokenFile,
		}, cfg)
		if err != nil {
			return nil, err
		}
		return &Connection{Backend: execution.WithoutEgress(d), Collectors: d.Collectors(), closer: d}, nil
	}
	b, err := e.OpenShell.Dial()
	if err != nil {
		return nil, err
	}
	if e.Owner != "" {
		b.WithOwner(execution.OwnerID(e.Owner))
	}
	return &Connection{Backend: b, closer: b}, nil
}
