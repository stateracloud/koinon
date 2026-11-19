// SPDX-License-Identifier: Apache-2.0

package preflight

// The user-namespace check of the gVisor class (issue #1118). gVisor (runsc) starts its sentry in a user
// namespace. A node with user.max_user_namespaces=0 (the Talos default) refuses that: runsc create exits 128 with
// "fork/exec .../gvisor-sentry-prewarmer: no space left on device". The disk is not full: the kernel returns
// ENOSPC because it allows no user namespace. The check reads the value from a short pod on each worker node.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	// SubstrateUserNSProbeMarker starts the user-namespace probe's output line.
	SubstrateUserNSProbeMarker = "koinon-userns-probe"
	// MaxUserNamespacesPath is the sysctl the probe reads.
	MaxUserNamespacesPath = "/proc/sys/user/max_user_namespaces"
	// TalosUserNamespaces is the value the fix names. It is the value of the cluster that confirmed the
	// fix (#1118); any value above 0 lets the sentry start.
	TalosUserNamespaces = 11255
)

// NodeUserNamespaces is what the probe pod of one node saw.
type NodeUserNamespaces struct {
	Node string
	// Max is the node's user.max_user_namespaces.
	Max int64
	// Skipped is why the node was not probed: it is not Ready, or it is cordoned or tainted and no node selector
	// names it. Such a node does not run the gVisor class.
	Skipped string
	// Err is why the node could not be probed (the pod did not run, or its log has no probe line).
	Err error
}

// UserNamespacesProber reads user.max_user_namespaces on each node that can run the gVisor class.
type UserNamespacesProber interface {
	ProbeUserNamespaces(ctx context.Context) ([]NodeUserNamespaces, error)
}

// UserNamespacesLine formats the probe line.
func UserNamespacesLine(max int64) string {
	return fmt.Sprintf("%s max=%d", SubstrateUserNSProbeMarker, max)
}

// ReadMaxUserNamespaces is what the probe pod runs: it reads the sysctl at path. A kernel built without user
// namespaces has no such file; the sentry cannot start there either, so that is reported as 0.
func ReadMaxUserNamespaces(path string) (int64, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the probe passes the fixed sysctl path; tests pass a temp file
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}

// ParseUserNamespacesOutput reads the probe line from the probe pod's log.
func ParseUserNamespacesOutput(log string) (int64, error) {
	for _, line := range strings.Split(log, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), SubstrateUserNSProbeMarker+" max=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed probe output %q", truncate(line, 200))
		}
		return n, nil
	}
	return 0, fmt.Errorf("no %q line in the probe pod's log: %q", SubstrateUserNSProbeMarker, truncate(log, 200))
}

// UserNamespacesPod is the probe pod of one node. It mounts nothing: /proc/sys/user is the node kernel's value
// for a pod in the initial user namespace. It names the node (the scheduler is not involved) and tolerates every
// taint, so a node that the operator selected by label is probed even when the worker pool taints it.
func (p KubeSubstrateProber) UserNamespacesPod(node string) *corev1.Pod {
	pod := p.pod("userns-probe", []string{"preflight", "userns-probe"}, nil, nil)
	pod.Spec.NodeName = node
	pod.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	return pod
}

// skipReason says why a node is not probed, or "". A node that is not Ready never runs a pod. Without a node
// selector the check cannot tell the worker nodes from the others, so it leaves out the nodes that the
// scheduler would not use for a plain pod (cordoned, or with a NoSchedule or NoExecute taint: the control plane).
// A node selector is the operator's statement of where gVisor runs, and it is probed even when tainted.
func (p KubeSubstrateProber) skipReason(node *corev1.Node) string {
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return "not Ready"
	}
	if len(p.NodeSelector) > 0 {
		return ""
	}
	if node.Spec.Unschedulable {
		return "cordoned"
	}
	for _, t := range node.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			return "tainted " + t.Key
		}
	}
	return ""
}

// ProbeUserNamespaces runs one probe pod on each node that matches NodeSelector, one after the other (the
// probe pods share their labels, and the stale-pod cleanup of one run must not remove the pod of another).
func (p KubeSubstrateProber) ProbeUserNamespaces(ctx context.Context) ([]NodeUserNamespaces, error) {
	nodes, err := p.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(p.NodeSelector).String()})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no node matches the node selector %q", labels.SelectorFromSet(p.NodeSelector).String())
	}
	var out []NodeUserNamespaces
	for _, node := range nodes.Items {
		res := NodeUserNamespaces{Node: node.Name}
		if why := p.skipReason(&node); why != "" {
			res.Skipped = why
			out = append(out, res)
			continue
		}
		log, err := p.run(ctx, p.UserNamespacesPod(node.Name))
		if err == nil {
			res.Max, err = ParseUserNamespacesOutput(log)
		}
		res.Err = err
		out = append(out, res)
	}
	return out, nil
}

func (c *Checker) checkUserNamespaces(ctx context.Context) Result {
	const check = "Substrate gVisor user namespaces"
	sub := c.Config.Substrate
	if !sub.GVisor {
		return Result{Check: check, Status: Pass, Message: "not checked: the gVisor sandbox class is not in use (the microVM tier needs no user namespaces)"}
	}
	if c.UserNS == nil {
		return Result{Check: check, Status: Warn, Message: "the user-namespace probe is disabled; user.max_user_namespaces on the gVisor nodes is not checked"}
	}
	nodes, err := c.UserNS.ProbeUserNamespaces(ctx)
	if err != nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("the user-namespace probe could not run in namespace %s: %v", sub.Namespace, err),
			Fix:     "fix the cause above (the namespace, Pod Security, image pull, the node selector or scheduling), then run the check again",
		}
	}
	var zero, broken, skipped []string
	probed := 0
	for _, n := range nodes {
		switch {
		case n.Skipped != "":
			skipped = append(skipped, n.Node+" ("+n.Skipped+")")
			continue
		}
		probed++
		switch {
		case n.Err != nil:
			broken = append(broken, fmt.Sprintf("%s: %v", n.Node, n.Err))
		case n.Max == 0:
			zero = append(zero, n.Node)
		}
	}
	if len(broken) > 0 {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("the user-namespace probe pod did not report on %d of %d nodes: %s", len(broken), probed, strings.Join(broken, "; ")),
			Fix:     "this is not a sysctl finding: fix the cause above (Pod Security, image pull, scheduling or the node), then run the check again",
		}
	}
	if len(zero) > 0 {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("user.max_user_namespaces is 0 on %s: gVisor starts its sentry in a user namespace, so every agent start on those nodes "+
				"fails with \"runsc create\" exit 128 and \"fork/exec .../gvisor-sentry-prewarmer: no space left on device\" (the disk is not full: "+
				"the kernel returns ENOSPC because it allows no user namespace)", strings.Join(zero, ", ")),
			Fix: UserNamespacesFix,
		}
	}
	note := ""
	if len(skipped) > 0 {
		note = "; not probed: " + strings.Join(skipped, ", ")
	}
	if probed == 0 {
		return Result{
			Check: check, Status: Warn, Message: "no node could be probed" + note,
			Fix: "pass --substrate-node-selector with the labels of the gVisor worker nodes, and make sure they are Ready",
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("user.max_user_namespaces is above 0 on %d node(s)%s", probed, note)}
}

// UserNamespacesFix is the Fix text of the failed check.
var UserNamespacesFix = fmt.Sprintf(
	"Set user.max_user_namespaces above 0 on the worker nodes that run gVisor sandboxes. Talos (default 0): add a machine config patch for the workers "+
		"(machine.sysctls: {user.max_user_namespaces: \"%[1]d\"}) and apply it with talosctl patch machineconfig --nodes <worker nodes> --patch @userns.yaml. "+
		"Other Linux nodes (kubeadm and others): sysctl -w user.max_user_namespaces=%[1]d, and keep it in /etc/sysctl.d/99-gvisor.conf. "+
		"Cost: user namespaces widen the kernel attack surface for every pod on those nodes (an unprivileged process can then reach kernel code that needs "+
		"privilege otherwise). Set it on the gVisor worker nodes only, or use the vm tier. See docs/dev/substrate-install.md, section \"Talos: user namespaces for gVisor\".",
	TalosUserNamespaces)
