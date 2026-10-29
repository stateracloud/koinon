// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"errors"
	"io"
	"strings"
	"testing/iotest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
		f, ok, err := standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T12:57:00Z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(f.Reason).To(Equal("unknown field amoun"))
		Expect(f.At).To(Equal(at("2026-10-29T12:59:18.680992Z")))
	})

	It("ignores failures logged before the given time and lines that are not reload failures", func() {
		_, ok, err := standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T13:00:00Z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		_, ok, err = standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T12:59:18.680992Z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "strictly after: the failure the baseline saw does not count")
		f, ok, err := standalone.ParseReloadFailure(strings.NewReader(log), at("2026-10-29T12:58:00Z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(f.Reason).To(Equal("unknown field amoun"))
	})

	It("skips a line over the cap and keeps reading after it", func() {
		huge := `{"time":"2026-10-29T12:58:00Z","message":"access ` + strings.Repeat("x", 3<<20) + `"}`
		input := huge + "\n" + `{"time":"2026-10-29T12:59:00Z","message":"Failed to reload config: after the long line"}` + "\n"
		f, ok, err := standalone.ParseReloadFailure(strings.NewReader(input), time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(f.Reason).To(Equal("after the long line"))
	})

	It("reads a final line without a newline and returns a read error", func() {
		f, ok, err := standalone.ParseReloadFailure(strings.NewReader(`{"time":"2026-10-29T12:59:00Z","message":"Failed to reload config: last"}`), time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(f.Reason).To(Equal("last"))
		_, _, err = standalone.ParseReloadFailure(io.MultiReader(strings.NewReader("x\n"), iotest.ErrReader(errors.New("boom"))), time.Time{})
		Expect(err).To(MatchError("boom"))
	})

	It("caps a long reason", func() {
		long := `{"time":"2026-10-29T12:58:00Z","message":"Failed to reload config: ` + strings.Repeat("x", 2000) + `"}`
		f, ok, err := standalone.ParseReloadFailure(strings.NewReader(long), time.Time{})
		Expect(err).NotTo(HaveOccurred())
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

	It("names the agentgateway container and honours the selector's expressions", func(ctx SpecContext) {
		client := fake.NewSimpleClientset(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "koinon-gateway", Namespace: "koinon"},
				Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"gateway"}}},
				}},
			},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gw-1", Namespace: "koinon", Labels: map[string]string{"app": "gateway"}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "koinon", Labels: map[string]string{"app": "other"}}},
		)
		var logged []string
		client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if g, ok := action.(k8stesting.GenericAction); ok && action.GetSubresource() == "log" {
				if o, ok := g.GetValue().(*corev1.PodLogOptions); ok {
					logged = append(logged, o.Container)
				}
			}
			return false, nil, nil
		})
		_, _, err := standalone.PodLogReload{Client: client, Namespace: "koinon", Deployment: "koinon-gateway"}.LatestReloadFailure(ctx, time.Now())
		Expect(err).NotTo(HaveOccurred())
		_, _, err = standalone.PodLogReload{Client: client, Namespace: "koinon", Deployment: "koinon-gateway", Container: "gw"}.LatestReloadFailure(ctx, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(logged).To(Equal([]string{"agentgateway", "gw"}), "one read per call: only the pod the selector matches")
	})
})
