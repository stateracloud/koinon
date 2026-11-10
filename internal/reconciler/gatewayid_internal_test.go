// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/preflight"
)

type stubGatewayID struct {
	id  string
	err error
}

func (s *stubGatewayID) GatewayID(context.Context) (string, error) { return s.id, s.err }

func gatewayIDReconciler(want string, src GatewayIDSource) *Reconciler {
	return &Reconciler{
		cfg: Config{GatewayID: want, GatewayIDs: src},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		m:   NewMetrics(nil),
	}
}

// Old issue 475: a gateway ID that changes after install leaves the policies matching nothing; the
// reconciler says so on its first pass and on every resync, and clears it when the IDs agree again.
func TestGatewayIDMismatchMetric(t *testing.T) {
	src := &stubGatewayID{id: "openshell"}
	r := gatewayIDReconciler("openshell", src)
	gauge := func() float64 { return testutil.ToFloat64(r.m.GatewayIDMismatch) }

	r.checkGatewayID(t.Context())
	if gauge() != 0 {
		t.Fatalf("matching IDs: mismatch = %v, want 0", gauge())
	}
	src.id = "prod"
	r.checkGatewayID(t.Context())
	if gauge() != 1 {
		t.Fatalf("gateway_id changed: mismatch = %v, want 1", gauge())
	}
	src.id, src.err = "", errors.New("forbidden")
	r.checkGatewayID(t.Context())
	if gauge() != 1 {
		t.Fatalf("an unreadable ConfigMap must keep the last answer: mismatch = %v, want 1", gauge())
	}
	src.id, src.err = "openshell", nil
	r.checkGatewayID(t.Context())
	if gauge() != 0 {
		t.Fatalf("IDs agree again: mismatch = %v, want 0", gauge())
	}
}

func TestGatewayIDCheckIsOffWithoutASource(t *testing.T) {
	r := gatewayIDReconciler("openshell", nil)
	r.checkGatewayID(t.Context())
	if v := testutil.ToFloat64(r.m.GatewayIDMismatch); v != 0 {
		t.Errorf("no source: mismatch = %v, want 0", v)
	}
	r = gatewayIDReconciler("", &stubGatewayID{id: "prod"})
	r.checkGatewayID(t.Context())
	if v := testutil.ToFloat64(r.m.GatewayIDMismatch); v != 0 {
		t.Errorf("no configured ID: mismatch = %v, want 0", v)
	}
}

func TestKubeGatewayIDReadsTheGatewayConfigMap(t *testing.T) {
	cm := func(toml string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "openshell", Name: "openshell-config"},
			Data:       map[string]string{preflight.GatewayConfigKey: toml},
		}
	}
	for name, tc := range map[string]struct{ toml, want string }{
		"set":   {"[openshell.drivers.kubernetes]\ngateway_id = \"prod\"\n", "prod"},
		"unset": {"[openshell.drivers.kubernetes]\nnamespace = \"openshell\"\n", "openshell"},
	} {
		src := KubeGatewayID{Client: fake.NewClientset(cm(tc.toml)), Namespace: "openshell", ConfigMap: "openshell-config"}
		got, err := src.GatewayID(t.Context())
		if err != nil || got != tc.want {
			t.Errorf("%s: GatewayID = %q, %v; want %q", name, got, err, tc.want)
		}
	}
	missing := KubeGatewayID{Client: fake.NewClientset(), Namespace: "openshell", ConfigMap: "openshell-config"}
	if _, err := missing.GatewayID(t.Context()); err == nil {
		t.Error("a missing ConfigMap must be an error")
	}
}
