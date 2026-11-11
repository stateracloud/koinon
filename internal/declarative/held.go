// SPDX-License-Identifier: Apache-2.0

package declarative

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	"github.com/stateracloud/koinon/internal/connections"
)

// ReasonHeldNotApplied is the Event reason of a Secret that a held install read but did not apply
// (ADR 0110 decision 1).
const ReasonHeldNotApplied = "HeldNotApplied"

// Plan actions. The set is fixed: it labels koinon_declarative_held_changes (ADR 0023).
const (
	ActionApply  = "apply"
	ActionRemove = "remove"
)

var heldKinds = []string{TypeModelProvider, TypeMCPServer, TypeConnection}

// heldChanges is koinon_declarative_held_changes{kind,action}: what the last pass of a held importer
// would have applied or removed. Fixed label sets, no org or Secret names (ADR 0023).
var heldChanges = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "koinon_declarative_held_changes",
	Help: "Declarative changes the importer found but did not apply because the install is held (restore-hold), by kind and action.",
}, []string{"kind", "action"})

// RegisterMetrics registers the importer's metrics on reg.
func RegisterMetrics(reg prometheus.Registerer) {
	for _, k := range heldKinds {
		for _, a := range []string{ActionApply, ActionRemove} {
			heldChanges.WithLabelValues(k, a).Set(0)
		}
	}
	reg.MustRegister(heldChanges)
}

// planSecret is the held counterpart of the apply functions: it parses the Secret with the same
// parser (so an invalid Secret is reported the same way) and records that it would be applied.
// It reads nothing from the database and writes nothing to it, and it dials nothing.
//
// Every valid Secret counts as an apply: the importer cannot tell a changed item from an unchanged
// one without the writes' own comparison, and an unchanged one is a no-op at the release.
func (i *Importer) planSecret(ctx context.Context, sec *corev1.Secret, typ, version string) error {
	var err error
	switch typ {
	case TypeModelProvider:
		_, err = ParseProvider(sec, version)
	case TypeMCPServer:
		_, err = ParseMCPServer(sec, version)
	case TypeConnection:
		_, err = ParseConnection(sec, version)
		if err == nil && i.Sealer == nil {
			err = connections.ErrNoKeyring
		}
	}
	if err != nil {
		if errors.Is(err, connections.ErrNoKeyring) {
			return err
		}
		return &invalidError{err}
	}
	if _, err := i.declaredDisabled(sec); err != nil {
		return err
	}
	if i.markPlanned(typ, ActionApply, sec.Labels[LabelOrg], sec.Name, version) {
		i.event(ctx, sec, corev1.EventTypeNormal, ReasonHeldNotApplied,
			"Held (restore-hold): this Secret is not applied until the hold is released")
	}
	return nil
}

// planned records a change the held importer would make and logs it once per version.
func (i *Importer) planned(kind, action, org, name string) {
	i.markPlanned(kind, action, org, name, "")
}

// markPlanned counts the change for the metric of the current pass and reports whether it is new
// since the last pass (the log line and the Event are written once per change, not on every resync).
func (i *Importer) markPlanned(kind, action, org, name, version string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.planPass == nil {
		i.planPass = map[[2]string]int{}
	}
	i.planPass[[2]string{kind, action}]++
	key := fmt.Sprintf("%s/%s/%s/%s", kind, action, org, name)
	if i.planSeen == nil {
		i.planSeen = map[string]string{}
	}
	if v, ok := i.planSeen[key]; ok && v == version {
		return false
	}
	i.planSeen[key] = version
	i.log().Info(fmt.Sprintf("held: would %s %s %s", action, kind, name), "org", org, "secret", name)
	return true
}

// beginPlan starts a held pass; endPlan publishes its counts and forgets the changes that are gone.
func (i *Importer) beginPlan() {
	i.mu.Lock()
	i.planPass = map[[2]string]int{}
	i.mu.Unlock()
}

func (i *Importer) endPlan() {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, k := range heldKinds {
		for _, a := range []string{ActionApply, ActionRemove} {
			heldChanges.WithLabelValues(k, a).Set(float64(i.planPass[[2]string{k, a}]))
		}
	}
}

// HeldPlan returns the counts of the last held pass by kind and action; tests read it.
func (i *Importer) HeldPlan(kind, action string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planPass[[2]string{kind, action}]
}
