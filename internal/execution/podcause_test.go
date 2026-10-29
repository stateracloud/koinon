// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	"encoding/json"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stateracloud/koinon/internal/execution"
)

func waiting(name, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}
}

var _ = Describe("PodWaitDetail", func() {
	It("explains the captured missing image as an image pull", func() {
		// initContainerStatuses of pod wc-img, captured from OpenShell 0.1.5 and Agent Sandbox v1.0.7 on
		// kind, 2026-10-29 (testdata/wc-img-init-container-statuses.json).
		data, err := os.ReadFile("testdata/wc-img-init-container-statuses.json")
		Expect(err).NotTo(HaveOccurred())
		pod := &corev1.Pod{}
		Expect(json.Unmarshal(data, &pod.Status.InitContainerStatuses)).To(Succeed())

		d := execution.PodWaitDetail(pod)
		Expect(d.Reason).To(Equal("ImagePullBackOff"))
		Expect(d.Message).To(HavePrefix(`Back-off pulling image "kind.local/koinon-escape/agent:nope": ErrImagePull:`))
		Expect(len(d.Message)).To(BeNumerically("<=", execution.MaxWaitDetail))

		// OpenShell's own words for this sandbox classify as unknown; with the pod's detail the cause is
		// image_pull.
		const osReason, osMessage = "ConfigurationPending", "Waiting for effective configuration validation before workload activation"
		Expect(execution.ClassifySandboxWait(osReason, osMessage)).To(Equal(execution.WaitCauseUnknown))
		reason, message := execution.ExplainWait(osReason, osMessage, d)
		Expect(execution.ClassifySandboxWait(reason, message)).To(Equal(execution.WaitCauseImagePull))
	})

	It("looks at init containers before containers and skips routine waits", func() {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{waiting("a", "PodInitializing", ""), waiting("b", "ErrImagePull", "boom")},
			ContainerStatuses:     []corev1.ContainerStatus{waiting("c", "CrashLoopBackOff", "back-off")},
		}}
		Expect(execution.PodWaitDetail(pod)).To(Equal(execution.WaitDetail{Reason: "ErrImagePull", Message: "boom"}))
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting("a", "ContainerCreating", "")}
		Expect(execution.PodWaitDetail(pod).Reason).To(Equal("CrashLoopBackOff"))
	})

	It("names a crash loop with its last exit", func() {
		cs := waiting("agent", "CrashLoopBackOff", "")
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}
		d := execution.PodWaitDetail(&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}}})
		Expect(d.Message).To(Equal(`container "agent" is in CrashLoopBackOff (last exit: Error)`))
		Expect(execution.ClassifySandboxWait(d.Reason, d.Message)).To(Equal(execution.WaitCauseCrashLoop))
	})

	It("says nothing for a pod that is only starting, and for nil", func() {
		Expect(execution.PodWaitDetail(nil).IsZero()).To(BeTrue())
		pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waiting("a", "PodInitializing", "")}}}
		Expect(execution.PodWaitDetail(pod).IsZero()).To(BeTrue())
	})

	It("cuts a long message on a rune boundary", func() {
		pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			waiting("c", "ErrImagePull", strings.Repeat("é", 1000)),
		}}}
		d := execution.PodWaitDetail(pod)
		Expect(len(d.Message)).To(BeNumerically("<=", execution.MaxWaitDetail))
		Expect(d.Message).To(HaveSuffix("..."))
		Expect(strings.ToValidUTF8(d.Message, "")).To(Equal(d.Message))
	})
})

var _ = Describe("SandboxCRDetail", func() {
	cr := func(reason, msg string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Suspended", "status": "False", "reason": "NotSuspended"},
			map[string]any{"type": "Ready", "status": "False", "reason": reason, "message": msg},
		}}}}
	}

	It("reads an admission refusal from the Ready condition", func() {
		// The message is the API server's text captured from the chart's policy (waitcause-capture.md,
		// 3c) inside the controller's "Error seen:" wrapper, whose format is from Agent Sandbox's source.
		msg := `Error seen: pods "x" is forbidden: ValidatingAdmissionPolicy 'koinon-koinon-sandbox-pods' with binding 'koinon-koinon-sandbox-pods' denied request: no`
		d := execution.SandboxCRDetail(cr("ReconcilerError", msg))
		Expect(d.Reason).To(Equal("ReconcilerError"))
		Expect(execution.ClassifySandboxWait(d.Reason, d.Message)).To(Equal(execution.WaitCauseAdmission))
	})

	It("reads the v1.0.8 container issue", func() {
		// Format from Agent Sandbox v1.0.8, controllers/sandbox_controller.go podContainerIssue.
		d := execution.SandboxCRDetail(cr("DependenciesNotReady", `Pod exists with phase: Pending; init container "workspace-init" is waiting: ImagePullBackOff`))
		Expect(execution.ClassifySandboxWait(d.Reason, d.Message)).To(Equal(execution.WaitCauseImagePull))
		d = execution.SandboxCRDetail(cr("DependenciesNotReady", `Pod exists with phase: Running; container "agent" is in CrashLoopBackOff (last exit: Error)`))
		Expect(execution.ClassifySandboxWait(d.Reason, d.Message)).To(Equal(execution.WaitCauseCrashLoop))
	})

	It("ignores the captured v1.0.7 message and a Ready condition that is True", func() {
		Expect(execution.SandboxCRDetail(cr("DependenciesNotReady", "Pod exists with phase: Pending")).IsZero()).To(BeTrue())
		ready := cr("DependenciesReady", "Pod is Ready")
		ready.Object["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["status"] = "True"
		Expect(execution.SandboxCRDetail(ready).IsZero()).To(BeTrue())
		Expect(execution.SandboxCRDetail(nil).IsZero()).To(BeTrue())
	})
})

var _ = DescribeTable("ExplainWait",
	func(reason, message string, d execution.WaitDetail, wantReason, wantMessage string) {
		r, m := execution.ExplainWait(reason, message, d)
		Expect([]string{r, m}).To(Equal([]string{wantReason, wantMessage}))
	},
	Entry("an opaque OpenShell reason is replaced", "ConfigurationPending", "waiting", execution.WaitDetail{Reason: "ErrImagePull", Message: "m"}, "ErrImagePull", "m"),
	Entry("Agent Sandbox's pending reason is replaced", "DependenciesNotReady", "Pod exists with phase: Pending", execution.WaitDetail{Reason: "X", Message: "m"}, "X", "m"),
	Entry("an informative reason is kept", "Unschedulable", "0/1 nodes", execution.WaitDetail{Reason: "X", Message: "m"}, "Unschedulable", "0/1 nodes"),
	Entry("an empty detail changes nothing", "ConfigurationPending", "waiting", execution.WaitDetail{}, "ConfigurationPending", "waiting"),
)

var _ = DescribeTable("WorkspaceNamespace",
	func(id, ws, want string) { Expect(execution.WorkspaceNamespace(id, ws)).To(Equal(want)) },
	// Captured: namespace openshell-openshell-kw-ff625e7f210d54d1 for gateway_id openshell.
	Entry("the default gateway", "openshell", "kw-ff625e7f210d54d1", "openshell-openshell-kw-ff625e7f210d54d1"),
	Entry("another gateway", "prod", "kw-1", "openshell-prod-kw-1"),
	Entry("an empty gateway ID is OpenShell's default", "", "kw-1", "openshell-openshell-kw-1"),
)
