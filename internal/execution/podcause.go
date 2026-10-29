// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"fmt"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// MaxWaitDetail bounds the message PodWaitDetail and SandboxCRDetail return, in bytes. A kubelet pull
// error can run to several kilobytes.
const MaxWaitDetail = 512

// WaitDetail is the reason and message that explain why a sandbox's pod cannot start, as the
// kubelet or the Agent Sandbox controller wrote them. The sandbox observer (ADR 0078) reads them
// when OpenShell's own condition says nothing useful.
type WaitDetail struct {
	Reason  string
	Message string
}

// IsZero reports whether the detail says nothing.
func (d WaitDetail) IsZero() bool { return d.Reason == "" && d.Message == "" }

// OpaqueWaitReason reports whether OpenShell's reason hides the cause. OpenShell replaces the Agent
// Sandbox controller's Ready condition with ConfigurationPending until the supervisor has started,
// which a pod that cannot start never does; Agent Sandbox itself reports DependenciesNotReady while
// the pod is pending. SchedulingGated is what OpenShell reports in the first seconds.
func OpaqueWaitReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "configurationpending", "dependenciesnotready", "schedulinggated":
		return true
	}
	return false
}

// ExplainWait returns the reason and message to record: the pod detail when OpenShell's reason is
// opaque and the detail says something, else OpenShell's own.
func ExplainWait(reason, message string, d WaitDetail) (string, string) {
	if d.IsZero() || !OpaqueWaitReason(reason) {
		return reason, message
	}
	return d.Reason, d.Message
}

// PodWaitDetail names why a pod cannot start from its container statuses: the first init container
// or container that waits for a reason other than the routine ContainerCreating and PodInitializing.
// Init containers come first because the pod does not start the others before they finish. A crash
// loop carries the last exit. It returns the zero detail when nothing explains the wait.
func PodWaitDetail(pod *corev1.Pod) WaitDetail {
	if pod == nil {
		return WaitDetail{}
	}
	for _, group := range []struct {
		kind     string
		statuses []corev1.ContainerStatus
	}{{"init container", pod.Status.InitContainerStatuses}, {"container", pod.Status.ContainerStatuses}} {
		for _, cs := range group.statuses {
			w := cs.State.Waiting
			if w == nil || w.Reason == "" || w.Reason == "ContainerCreating" || w.Reason == "PodInitializing" {
				continue
			}
			msg := w.Message
			if t := cs.LastTerminationState.Terminated; t != nil && w.Reason == "CrashLoopBackOff" {
				exit := t.Reason
				if exit == "" {
					exit = fmt.Sprintf("exit code %d", t.ExitCode)
				}
				if msg == "" {
					msg = fmt.Sprintf("%s %q is in CrashLoopBackOff (last exit: %s)", group.kind, cs.Name, exit)
				} else {
					msg = fmt.Sprintf("%s %q: %s (last exit: %s)", group.kind, cs.Name, msg, exit)
				}
			}
			return WaitDetail{Reason: w.Reason, Message: truncate(msg, MaxWaitDetail)}
		}
	}
	return WaitDetail{}
}

// SandboxCRDetail reads the Ready=False condition of an Agent Sandbox object, for the case that its
// pod does not exist: the controller writes the API server's refusal there, for example
// ReconcilerError "Error seen: pods ... is forbidden: ValidatingAdmissionPolicy ...". It returns
// the zero detail when Ready is True or missing, or when its reason is as opaque as OpenShell's.
func SandboxCRDetail(sb *unstructured.Unstructured) WaitDetail {
	if sb == nil {
		return WaitDetail{}
	}
	conds, _, _ := unstructured.NestedSlice(sb.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" || m["status"] != "False" {
			continue
		}
		reason, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		// "Pod exists with phase: Pending" (v1.0.7) says nothing the pod does not say better.
		if OpaqueWaitReason(reason) && !strings.Contains(msg, ";") {
			return WaitDetail{}
		}
		return WaitDetail{Reason: reason, Message: truncate(msg, MaxWaitDetail)}
	}
	return WaitDetail{}
}

// truncate cuts s to at most n bytes on a rune boundary, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "..."
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}
