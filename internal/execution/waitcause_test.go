// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

var _ = DescribeTable("ClassifySandboxWait",
	func(reason, message string, want execution.SandboxWaitCause) {
		Expect(execution.ClassifySandboxWait(reason, message)).To(Equal(want))
	},
	Entry("an image pull reason", "ErrImagePull", "", execution.WaitCauseImagePull),
	Entry("a back-off reason", "ImagePullBackOff", "", execution.WaitCauseImagePull),
	Entry("a pull error in the message", "", `failed to pull image "registry.example.test/x:1": pull access denied`, execution.WaitCauseImagePull),
	Entry("an admission webhook", "", `admission webhook "x" denied the request: no`, execution.WaitCauseAdmission),
	Entry("a ValidatingAdmissionPolicy", "", `pods "x" is forbidden: ValidatingAdmissionPolicy 'koinon-sandbox' with binding 'b' denied request: no`, execution.WaitCauseAdmission),
	Entry("a quota", "", "exceeded quota: compute", execution.WaitCauseAdmission),
	Entry("an unschedulable pod", "Unschedulable", "", execution.WaitCauseNoNodes),
	Entry("no node has the memory", "", "0/3 nodes are available: 3 Insufficient memory.", execution.WaitCauseNoNodes),
	Entry("nothing", "", "", execution.WaitCauseUnknown),
	Entry("a reason that says nothing", "Pending", "waiting", execution.WaitCauseUnknown),
	Entry("a reason in other case", "errimagepull", "", execution.WaitCauseImagePull),
	// Captured from OpenShell 0.1.5 and Agent Sandbox v1.0.7 on kind, 2026-10-29: the first condition
	// that is not True in OpenShell's sandbox status, which is what the classifier is given.
	Entry("captured: no node has the CPU", "Unschedulable",
		"0/1 nodes are available: 1 Insufficient cpu. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.",
		execution.WaitCauseNoNodes),
	// With a flavor image that cannot be pulled, OpenShell reports no pull error in its conditions: its
	// ConfigurationReady condition stays False and the pod's ImagePullBackOff shows only in the pod's
	// container status and events. These two entries pin what the classifier does with OpenShell's
	// words alone (the sandbox observer, ADR 0078, off or unable to read): unknown. With the observer
	// the pod's detail replaces them (podcause_test.go: the same sandbox is image_pull).
	Entry("captured: OpenShell alone while the image cannot be pulled (no observer)", "ConfigurationPending",
		"Waiting for effective configuration validation before workload activation", execution.WaitCauseUnknown),
	Entry("captured: Agent Sandbox ready condition while the pod is pending (no observer)", "DependenciesNotReady",
		"Pod exists with phase: Pending", execution.WaitCauseUnknown),
	Entry("captured: OpenShell while the sandbox is scheduling-gated", "SchedulingGated",
		"Scheduling is blocked due to non-empty scheduling gates", execution.WaitCauseUnknown),
	// Not sandbox conditions, but the real kubelet and API server strings a pod-level source would carry.
	Entry("captured: kubelet waiting state of an unpullable image", "ImagePullBackOff",
		`Back-off pulling image "kind.local/koinon-escape/agent:nope": ErrImagePull: failed to pull and unpack image "kind.local/koinon-escape/agent:nope": failed to resolve reference "kind.local/koinon-escape/agent:nope": failed to do request: Head "https://kind.local/v2/koinon-escape/agent/manifests/nope": dial tcp: lookup kind.local on 0.250.250.254:53: server misbehaving`,
		execution.WaitCauseImagePull),
	Entry("captured: kubelet pull failure event", "Failed",
		`Failed to pull image "kind.local/koinon-escape/agent:nope": failed to pull and unpack image "kind.local/koinon-escape/agent:nope": failed to resolve reference "kind.local/koinon-escape/agent:nope": failed to do request: Head "https://kind.local/v2/koinon-escape/agent/manifests/nope": dial tcp: lookup kind.local on 0.250.250.254:53: server misbehaving`,
		execution.WaitCauseImagePull),
	Entry("a crash loop", "CrashLoopBackOff", "", execution.WaitCauseCrashLoop),
	Entry("a crash loop in the message", "", `container "agent" is in CrashLoopBackOff (last exit: Error)`, execution.WaitCauseCrashLoop),
	Entry("a container that cannot be created", "CreateContainerError", "", execution.WaitCauseContainerCreate),
	Entry("a container whose configuration is refused", "CreateContainerConfigError", `secret "x" not found`, execution.WaitCauseContainerCreate),
	Entry("a container the runtime cannot run", "RunContainerError", "", execution.WaitCauseContainerCreate),
	Entry("captured: the chart's ValidatingAdmissionPolicy refusing a pod", "",
		"The pods \"vap-probe\" is invalid: : ValidatingAdmissionPolicy 'koinon-koinon-sandbox-pods' with binding 'koinon-koinon-sandbox-pods' denied request: only the Agent Sandbox controller (and the OpenShell gateway, for supervisor pods) may create pods in sandbox namespaces, not kubernetes-admin",
		execution.WaitCauseAdmission),
)

var _ = DescribeTable("SandboxWaitCause.Describe",
	func(c execution.SandboxWaitCause, want string) { Expect(c.Describe()).To(Equal(want)) },
	Entry("image_pull", execution.WaitCauseImagePull, "the image could not be pulled"),
	Entry("admission", execution.WaitCauseAdmission, "the cluster's admission policy refused the sandbox"),
	Entry("no_nodes", execution.WaitCauseNoNodes, "no node can run the sandbox"),
	Entry("crash_loop", execution.WaitCauseCrashLoop, "a container in the sandbox keeps crashing"),
	Entry("container_create", execution.WaitCauseContainerCreate, "a container in the sandbox could not be created"),
	Entry("unknown", execution.WaitCauseUnknown, "the cause is unknown"),
)
