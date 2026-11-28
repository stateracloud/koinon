// SPDX-License-Identifier: Apache-2.0

package execution

import "context"

// WithoutEgress adapts a driver that has only the [Compute] half to the [Backend] the reconciler
// takes. Every [Egress] call succeeds and does nothing: a driver with no Egress half (Agent
// Substrate, epic 1082) has no provider records and no in-sandbox policy, because the Koinon gateway
// holds the credentials and the per-route rules (ADR 0097) and the execution layer's network is
// default-deny. EnsurePolicy reports version 0.
//
// CreateSandbox drops what the reconciler renders for the OpenShell path and a Compute-only driver
// has no place for (issue #1245): the provider records (no credential in the guest, ADR 0119 D2),
// the grant-derived network rules (egress is the gateway's, ADR 0097) and the per-sandbox filesystem
// policy (the guest launcher applies one fixed Landlock policy to every guest process, D1, so
// nothing is lost). The reconciler's SandboxPolicy and the launcher's default policy are compared
// in a reconciler spec. Resources, labels, image and runtime class pass through.
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

// CreateSandbox implements [Compute] without the fields of the Egress half.
func (n noEgress) CreateSandbox(ctx context.Context, workspace string, spec SandboxSpec) (Sandbox, error) {
	spec.Providers = nil
	spec.Policy = Policy{}
	return n.Compute.CreateSandbox(ctx, workspace, spec)
}

// SpecID forwards to the driver when it implements [SpecIdentifier] (a driver without labels).
func (n noEgress) SpecID(workspace string, spec SandboxSpec) (string, error) {
	if si, ok := n.Compute.(SpecIdentifier); ok {
		return si.SpecID(workspace, spec)
	}
	return "", nil
}

// IdentifiesSpecs reports whether the backend identifies a sandbox's spec by [SpecIdentifier] and
// not by labels. The adapter answers for its driver.
func IdentifiesSpecs(b any) bool {
	if n, ok := b.(noEgress); ok {
		b = n.Compute
	}
	_, ok := b.(SpecIdentifier)
	return ok
}

// strippedRepointer is the [Repointer] of a driver behind [WithoutEgress]: like CreateSandbox, it gives the
// driver a spec without the fields of the Egress half.
type strippedRepointer struct{ Repointer }

func (n strippedRepointer) RepointSandbox(ctx context.Context, workspace, name string, spec SandboxSpec) (Sandbox, error) {
	spec.Providers = nil
	spec.Policy = Policy{}
	return n.Repointer.RepointSandbox(ctx, workspace, name, spec)
}
