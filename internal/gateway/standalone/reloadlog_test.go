// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

var _ = Describe("ParseReloadFailure", func() {
	// Lines as agentgateway v1.7.0 writes them with logging.format json (rc.7 home lab, 2026-10-29).
	const log = `{"level":"info","time":"2026-10-29T12:56:50.830000Z","scope":"agentgateway::state_manager","message":"loaded config from File(\"/config/config.yaml\")"}
{"level":"error","time":"2026-10-29T12:58:00.815800Z","scope":"agentgateway::state_manager","message":"Failed to reload config: USD budget limits support at most 9 fractional digits"}
{"level":"error","time":"2026-10-29T12:58:01.000000Z","scope":"agentgateway::http::apikey","message":"request failed: Failed to reload config: not a failure"}
not json Failed to reload config: garbage
{"level":"error","time":"2026-10-29T12:59:18.680992Z","scope":"agentgateway::state_manager","message":"Failed to reload config: unknown field amoun"}
`
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }

	It("returns the latest failure logged since the given time", func() {
		f, ok := standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T12:57:00Z"))
		Expect(ok).To(BeTrue())
		Expect(f.Reason).To(Equal("unknown field amoun"))
		Expect(f.At).To(Equal(at("2026-10-29T12:59:18.680992Z")))
	})

	It("ignores failures logged before the given time and lines that are not reload failures", func() {
		_, ok := standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T13:00:00Z"))
		Expect(ok).To(BeFalse())
		_, ok = standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T12:59:18.680992Z"))
		Expect(ok).To(BeFalse(), "strictly after: the failure the baseline saw does not count")
		f, ok := standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T12:58:00Z"))
		Expect(ok).To(BeTrue())
		Expect(f.Reason).To(Equal("unknown field amoun"))
	})

	It("caps a long reason", func() {
		long := `{"time":"2026-10-29T12:58:00Z","message":"Failed to reload config: ` + strings.Repeat("x", 2000) + `"}`
		f, ok := standalone.ParseReloadFailure(strings.NewReader(long), time.Time{})
		Expect(ok).To(BeTrue())
		Expect(len(f.Reason)).To(BeNumerically("<", 600))
	})
})

var _ = Describe("PodLogReload", func() {
	It("lists the pods of the gateway Deployment's selector and reads their logs", func(ctx SpecContext) {
		client := fake.NewSimpleClientset(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "koinon-gateway", Namespace: "koinon"},
				Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}}},
			},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gw-1", Namespace: "koinon", Labels: map[string]string{"app": "gateway"}}},
		)
		r := standalone.PodLogReload{Client: client, Namespace: "koinon", Deployment: "koinon-gateway"}
		// The fake clientset serves the fixed log line "fake logs": no failure in it.
		_, found, err := r.LatestReloadFailure(ctx, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())

		_, _, err = standalone.PodLogReload{Client: client, Namespace: "koinon", Deployment: "missing"}.LatestReloadFailure(ctx, time.Now())
		Expect(err).To(HaveOccurred())
	})
})
