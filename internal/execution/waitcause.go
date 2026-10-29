// SPDX-License-Identifier: Apache-2.0

package execution

import "strings"

// SandboxWaitCause names why a sandbox never became ready (ADR 0066 amendment 2026-10-28; crash_loop and the sandbox observer, ADR 0078). A fixed
// set: it is a metric label.
type SandboxWaitCause string

const (
	WaitCauseImagePull SandboxWaitCause = "image_pull"
	WaitCauseAdmission SandboxWaitCause = "admission"
	WaitCauseNoNodes   SandboxWaitCause = "no_nodes"
	WaitCauseCrashLoop SandboxWaitCause = "crash_loop"
	WaitCauseUnknown   SandboxWaitCause = "unknown"
)

// WaitCauses lists every cause, for pre-initialising metric labels.
var WaitCauses = []SandboxWaitCause{WaitCauseImagePull, WaitCauseAdmission, WaitCauseNoNodes, WaitCauseCrashLoop, WaitCauseUnknown}

var (
	imagePullReasons = []string{"errimagepull", "imagepullbackoff", "invalidimagename", "errimageneverpull"}
	imagePullText    = []string{
		"imagepullbackoff", "errimagepull", "failed to pull image", "pull access denied",
		"manifest unknown", "image not found",
	}
	admissionText = []string{
		"admission webhook", "validatingadmissionpolicy", "denied request",
		"denied the request", "exceeded quota", "violates podsecurity",
	}
	crashLoopReasons = []string{"crashloopbackoff", "createcontainererror"}
	crashLoopText    = []string{"crashloopbackoff"}
	noNodesText      = []string{
		"nodes are available", "unschedulable", "insufficient cpu", "insufficient memory",
		"untolerated taint", "didn't match pod's node affinity",
	}
)

// ClassifySandboxWait names the cause from the latest condition's reason and message. The reason is
// matched exactly and the message by substring, both ignoring case; the first matching rule wins.
func ClassifySandboxWait(reason, message string) SandboxWaitCause {
	r, m := strings.ToLower(strings.TrimSpace(reason)), strings.ToLower(message)
	switch {
	case containsExact(imagePullReasons, r) || containsAny(m, imagePullText):
		return WaitCauseImagePull
	case containsAny(m, admissionText):
		return WaitCauseAdmission
	case r == "unschedulable" || containsAny(m, noNodesText):
		return WaitCauseNoNodes
	case containsExact(crashLoopReasons, r) || containsAny(m, crashLoopText):
		return WaitCauseCrashLoop
	}
	return WaitCauseUnknown
}

// Describe is the plain-words cause for messages.
func (c SandboxWaitCause) Describe() string {
	switch c {
	case WaitCauseImagePull:
		return "the image could not be pulled"
	case WaitCauseAdmission:
		return "the cluster's admission policy refused the sandbox"
	case WaitCauseNoNodes:
		return "no node can run the sandbox"
	case WaitCauseCrashLoop:
		return "a container in the sandbox keeps crashing"
	}
	return "the cause is unknown"
}

func containsExact(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
