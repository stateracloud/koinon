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

func TestSpecHashCoversTheSupervisorImage(t *testing.T) {
	spec := execution.SandboxSpec{Image: "img", Labels: map[string]string{}}
	none := specHash(spec, "")
	if got := specHash(spec, "supervisor:1"); got == none {
		t.Error("a supervisor image does not change the hash")
	}
	if specHash(spec, "supervisor:1") == specHash(spec, "supervisor:2") {
		t.Error("a new supervisor image does not change the hash")
	}
	if specHash(spec, "supervisor:1") != specHash(spec, "supervisor:1") {
		t.Error("the hash is not stable")
	}
}

func TestSandboxSpecHashFollowsTheGatewaySupervisor(t *testing.T) {
	src := &stubSupervisor{image: "supervisor:1"}
	r := testReconciler(src)
	r.refreshSupervisor(t.Context())
	_, h1 := r.sandboxSpec(zeroUUID, zeroUUID, execution.AgentNames{Sandbox: "sb"}, Flavor{Image: "img"}, nil, nil, defaultRes)
	src.image = "supervisor:2"
	r.refreshSupervisor(t.Context())
	_, h2 := r.sandboxSpec(zeroUUID, zeroUUID, execution.AgentNames{Sandbox: "sb"}, Flavor{Image: "img"}, nil, nil, defaultRes)
	if h1 == h2 {
		t.Error("a new supervisor image leaves the spec hash unchanged")
	}
}

func TestRefreshSupervisorKeepsTheLastImageOnError(t *testing.T) {
	src := &stubSupervisor{err: errors.New("forbidden")}
	r := testReconciler(src)
	r.refreshSupervisor(t.Context())
	if err := r.supervisorReady(); err == nil {
		t.Fatal("sandbox work should wait while the supervisor image has never been read")
	}
	src.image, src.err = "supervisor:1", nil
	r.refreshSupervisor(t.Context())
	if err := r.supervisorReady(); err != nil {
		t.Fatalf("ready after a read: %v", err)
	}
	src.image, src.err = "", errors.New("gone")
	r.refreshSupervisor(t.Context())
	if got := r.supervisorImage(); got != "supervisor:1" {
		t.Errorf("image after a failed read = %q, want the last one read", got)
	}
}

func TestNoSupervisorSourceIsAlwaysReady(t *testing.T) {
	r := testReconciler(nil)
	r.refreshSupervisor(t.Context())
	if err := r.supervisorReady(); err != nil || r.supervisorImage() != "" {
		t.Errorf("ready = %v, image = %q", err, r.supervisorImage())
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
