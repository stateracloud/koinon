// SPDX-License-Identifier: Apache-2.0

package execution

import "context"

// WithoutEgress adapts a driver that has only the [Compute] half to the [Backend] the reconciler
// takes. Every [Egress] call succeeds and does nothing: a driver with no Egress half (Agent
// Substrate, epic 1082) has no provider records and no in-sandbox policy, because the Koinon gateway
// holds the credentials and the per-route rules (ADR 0097) and the execution layer's network is
// default-deny. EnsurePolicy reports version 0.
//
// It is the one place that says "no egress", so the reconciler keeps one code path. It goes away
// when the OpenShell driver and the Egress half do (epic 1082, D9): the reconciler then takes
// [Compute].
func WithoutEgress(c Compute) Backend { return noEgress{Compute: c} }

type noEgress struct{ Compute }

// Unwrap returns the driver, so a caller can reach an optional capability ([Suspender],
// [Snapshotter]) that the adapter does not forward.
func (n noEgress) Unwrap() Compute { return n.Compute }

func (noEgress) EnsureProvider(context.Context, string, Provider) error      { return nil }
func (noEgress) DeleteProvider(context.Context, string, string) error        { return nil }
func (noEgress) DeleteProviderProfile(context.Context, string, string) error { return nil }
func (noEgress) EnsurePolicy(context.Context, string, string, Policy) (uint32, error) {
	return 0, nil
}
