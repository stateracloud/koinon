// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/templates"
)

type stubSupervisor struct {
	image string
	err   error
}

func (s *stubSupervisor) SupervisorImage(context.Context) (string, error) { return s.image, s.err }

var (
	zeroUUID   pgtype.UUID
	defaultRes = templates.Resources{
		CPU:    templates.Quantities{Request: "250m", Limit: "2"},
		Memory: templates.Quantities{Request: "1Gi", Limit: "2Gi"},
	}
)

func testReconciler(src SupervisorSource) *Reconciler {
	return &Reconciler{
		cfg: Config{Supervisor: src, MaxRecreatesPerPass: 2},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSpecHashLeavesTheSupervisorImageOut(t *testing.T) {
	src := &stubSupervisor{image: "supervisor:1"}
	r := testReconciler(src)
	r.refreshSupervisor(t.Context())
	names := execution.AgentNames{Sandbox: "sb"}
	s1, h1 := r.sandboxSpec(zeroUUID, zeroUUID, names, Harness{Image: "img"}, nil, nil, defaultRes)
	src.image = "supervisor:2"
	r.refreshSupervisor(t.Context())
	s2, h2 := r.sandboxSpec(zeroUUID, zeroUUID, names, Harness{Image: "img"}, nil, nil, defaultRes)
	if h1 != h2 {
		t.Error("a new supervisor image changes the spec hash")
	}
	if s1.Labels[LabelSupervisorImage] != supervisorDigest("supervisor:1") || s2.Labels[LabelSupervisorImage] != supervisorDigest("supervisor:2") || s1.Labels[LabelSupervisorImage] == s2.Labels[LabelSupervisorImage] {
		t.Errorf("supervisor labels = %q, %q", s1.Labels[LabelSupervisorImage], s2.Labels[LabelSupervisorImage])
	}
}

func TestSupervisorChanged(t *testing.T) {
	src := &stubSupervisor{err: errors.New("forbidden")}
	r := testReconciler(src)
	r.refreshSupervisor(t.Context())
	old := execution.Sandbox{Labels: map[string]string{LabelSupervisorImage: supervisorDigest("supervisor:1")}}
	if r.supervisorChanged(old) {
		t.Fatal("an unknown image must not drive a recreate")
	}
	if _, known := r.supervisorImage(); known {
		t.Fatal("known before any read")
	}
	src.image, src.err = "supervisor:1", nil
	r.refreshSupervisor(t.Context())
	if r.supervisorChanged(old) {
		t.Error("the same image is a change")
	}
	if !r.supervisorChanged(execution.Sandbox{Labels: map[string]string{}}) {
		t.Error("a sandbox without the label is not recreated once the image is known")
	}
	src.image = "supervisor:2"
	r.refreshSupervisor(t.Context())
	if !r.supervisorChanged(old) {
		t.Error("a new image is not a change")
	}
	src.image, src.err = "", errors.New("gone")
	r.refreshSupervisor(t.Context())
	if img, _ := r.supervisorImage(); img != "supervisor:2" {
		t.Errorf("image after a failed read = %q, want the last one read", img)
	}
}

func TestSupervisorLabelIsAValidLabelValue(t *testing.T) {
	long := "registry.example.internal:5000/some/long/organisation/path/openshell/supervisor-image:0.1.5-rc.1@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r := testReconciler(&stubSupervisor{image: long})
	r.refreshSupervisor(t.Context())
	spec, _ := r.sandboxSpec(zeroUUID, zeroUUID, execution.AgentNames{Sandbox: "sb"}, Harness{Image: "img"}, nil, nil, defaultRes)
	v := spec.Labels[LabelSupervisorImage]
	if errs := validation.IsValidLabelValue(v); len(errs) > 0 || v == "" || len(v) != 16 {
		t.Errorf("label value %q is not a short valid label value: %v", v, errs)
	}
	if r.supervisorChanged(execution.Sandbox{Labels: spec.Labels}) {
		t.Error("a sandbox created with the current image reads as changed")
	}
	if supervisorDigest("") != "" {
		t.Error("an empty image must stay empty")
	}
	r2 := testReconciler(nil)
	if r2.supervisorChanged(execution.Sandbox{Labels: map[string]string{}}) {
		t.Error("with no supervisor configured a sandbox without the label is recreated")
	}
}

func TestNoSupervisorSourceIsAlwaysReady(t *testing.T) {
	r := testReconciler(nil)
	r.refreshSupervisor(t.Context())
	if img, known := r.supervisorImage(); !known || img != "" {
		t.Errorf("known = %v, image = %q", known, img)
	}
}

func TestTakeRecreateCapsAPass(t *testing.T) {
	r := testReconciler(nil)
	got := []bool{r.takeRecreate(), r.takeRecreate(), r.takeRecreate()}
	if got[0] != true || got[1] != true || got[2] != false {
		t.Errorf("takes = %v, want two then none", got)
	}
	r.recreates.Store(0) // the next pass
	if !r.takeRecreate() {
		t.Error("a new pass has no slots")
	}
	r.cfg.MaxRecreatesPerPass = -1
	for range 100 {
		if !r.takeRecreate() {
			t.Fatal("a negative cap limits")
		}
	}
}

func TestKubeSupervisorImageReadsTheGatewayConfig(t *testing.T) {
	cm := func(data map[string]string) *fake.Clientset {
		return fake.NewClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "openshell", Name: "openshell-config"},
			Data:       data,
		})
	}
	toml := "[openshell.drivers.kubernetes]\nsupervisor_image = \"ghcr.io/nvidia/openshell/supervisor:0.1.5\" # pinned\n"
	src := KubeSupervisorImage{Client: cm(map[string]string{"gateway.toml": toml}), Namespace: "openshell", ConfigMap: "openshell-config"}
	got, err := src.SupervisorImage(t.Context())
	if err != nil || got != "ghcr.io/nvidia/openshell/supervisor:0.1.5" {
		t.Errorf("image = %q, err = %v", got, err)
	}
	// A gateway that sets none is not an error: the image stays out of the hash.
	src.Client = cm(map[string]string{"gateway.toml": "[openshell]\n"})
	if got, err := src.SupervisorImage(t.Context()); err != nil || got != "" {
		t.Errorf("unset image = %q, err = %v", got, err)
	}
	src.Client = cm(map[string]string{})
	if _, err := src.SupervisorImage(t.Context()); err == nil {
		t.Error("a ConfigMap without gateway.toml should be an error")
	}
	src.ConfigMap = "other"
	if _, err := src.SupervisorImage(t.Context()); err == nil {
		t.Error("a missing ConfigMap should be an error")
	}
}
