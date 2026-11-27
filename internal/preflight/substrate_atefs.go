// SPDX-License-Identifier: Apache-2.0

package preflight

// The filesystem check of /var/lib/ate (issue #1494). The home of a sandbox is a file or a directory under
// /var/lib/ate/actors/<uid> on the node, in every tier. Kubelet does not see that hostPath. A home that fills up
// fills the filesystem that holds /var/lib/ate, and kubelet reports 0 bytes used. v0.1 has no hard cap per home, so
// the protection of the node is a filesystem of its own for /var/lib/ate. The check warns when /var/lib/ate is on
// the same filesystem as the node root. It reads the device number of both mount points from a short pod on each node.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
)

const (
	// SubstrateAteFSProbeMarker starts the probe's output line.
	SubstrateAteFSProbeMarker = "koinon-atefs-probe"
	// AteDir is the base directory of the Substrate workers on a node.
	AteDir = "/var/lib/ate"
	// ateFSProbeRoot and ateFSProbeAte are the mount points of the node root and of AteDir in the probe pod.
	ateFSProbeRoot = "/probe/root"
	ateFSProbeAte  = "/probe/ate"
)

// NodeAteFS is what the probe pod of one node saw.
type NodeAteFS struct {
	Node string
	// SameAsRoot is true when AteDir and the node root are on one filesystem.
	SameAsRoot bool
	// Skipped is why the node was not probed (see skipReason).
	Skipped string
	// Err is why the node could not be probed.
	Err error
}

// AteFSProber finds out, on each node, whether AteDir has a filesystem of its own.
type AteFSProber interface {
	ProbeAteFS(ctx context.Context) ([]NodeAteFS, error)
}

// AteFSLine formats the probe line.
func AteFSLine(sameAsRoot bool) string {
	return fmt.Sprintf("%s same_as_root=%t", SubstrateAteFSProbeMarker, sameAsRoot)
}

// ParseAteFSOutput reads the probe line from the probe pod's log.
func ParseAteFSOutput(log string) (bool, error) {
	for _, line := range strings.Split(log, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), SubstrateAteFSProbeMarker+" same_as_root=")
		if !ok {
			continue
		}
		same, err := strconv.ParseBool(rest)
		if err != nil {
			return false, fmt.Errorf("malformed probe output %q", truncate(line, 200))
		}
		return same, nil
	}
	return false, fmt.Errorf("no %q line in the probe pod's log: %q", SubstrateAteFSProbeMarker, truncate(log, 200))
}

// AteFSPod is the probe pod of one node. It mounts the node root and AteDir read-only, and compares the device
// numbers of the two mount points. It names the node and tolerates every taint, as the user-namespace probe does.
// The AteDir volume is a plain Directory: a node whose workers never ran has no AteDir yet, and the pod then
// reports a mount error that the check turns into a warning.
func (p KubeSubstrateProber) AteFSPod(node string) *corev1.Pod {
	host := func(name, path string, t corev1.HostPathType) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: ptr.To(t)}}}
	}
	volumes := []corev1.Volume{host("root", "/", corev1.HostPathDirectory), host("ate", AteDir, corev1.HostPathDirectory)}
	mounts := []corev1.VolumeMount{
		{Name: "root", MountPath: ateFSProbeRoot, ReadOnly: true},
		{Name: "ate", MountPath: ateFSProbeAte, ReadOnly: true},
	}
	pod := p.pod("atefs-probe", []string{"preflight", "atefs-probe"}, volumes, mounts)
	pod.Spec.NodeName = node
	pod.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	return pod
}

// ProbeAteFS runs one probe pod on each node that matches NodeSelector, one after the other.
func (p KubeSubstrateProber) ProbeAteFS(ctx context.Context) ([]NodeAteFS, error) {
	nodes, err := p.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(p.NodeSelector).String()})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no node matches the node selector %q", labels.SelectorFromSet(p.NodeSelector).String())
	}
	var out []NodeAteFS
	for _, node := range nodes.Items {
		res := NodeAteFS{Node: node.Name}
		if why := p.skipReason(&node); why != "" {
			res.Skipped = why
			out = append(out, res)
			continue
		}
		log, err := p.run(ctx, p.AteFSPod(node.Name))
		if err == nil {
			res.SameAsRoot, err = ParseAteFSOutput(log)
		}
		res.Err = err
		out = append(out, res)
	}
	return out, nil
}

// AteFSFix is the Fix text of the warning.
const AteFSFix = "Put /var/lib/ate on its own filesystem on each worker node (a separate disk or partition, mounted before kubelet " +
	"starts), so that a full sandbox home fills that filesystem and not the root or the kubelet filesystem of the node. " +
	"Optionally put /var/lib/ate/snapshots on its own disk too. v0.1 has no hard cap per home (issue #1495). " +
	"See docs/dev/substrate-install.md, section \"Node protection: /var/lib/ate on its own filesystem\"."

func (c *Checker) checkAteFS(ctx context.Context) Result {
	const check = "Substrate /var/lib/ate filesystem"
	if c.AteFS == nil {
		return Result{Check: check, Status: Warn, Message: "the filesystem probe is disabled; whether /var/lib/ate has a filesystem of its own is not checked"}
	}
	nodes, err := c.AteFS.ProbeAteFS(ctx)
	if err != nil {
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("the /var/lib/ate probe could not run in namespace %s: %v", c.Config.Substrate.ProbeNamespace, err),
			Fix:     "fix the cause above (the namespace, Pod Security, image pull, the node selector or scheduling), then run the check again",
		}
	}
	var shared, broken, skipped []string
	probed := 0
	for _, n := range nodes {
		if n.Skipped != "" {
			skipped = append(skipped, n.Node+" ("+n.Skipped+")")
			continue
		}
		probed++
		switch {
		case n.Err != nil:
			broken = append(broken, fmt.Sprintf("%s: %v", n.Node, n.Err))
		case n.SameAsRoot:
			shared = append(shared, n.Node)
		}
	}
	note := ""
	if len(skipped) > 0 {
		note = "; not probed: " + strings.Join(skipped, ", ")
	}
	switch {
	case len(shared) > 0:
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("/var/lib/ate is on the same filesystem as the node root on %s: a sandbox home that fills up fills the node root, "+
				"and kubelet reports 0 bytes used%s", strings.Join(shared, ", "), note),
			Fix: AteFSFix,
		}
	case len(broken) > 0:
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("the /var/lib/ate probe pod did not report on %d of %d nodes: %s%s", len(broken), probed, strings.Join(broken, "; "), note),
			Fix:     "fix the cause above (a node without /var/lib/ate has run no worker yet: create the mount first), then run the check again",
		}
	case probed == 0:
		return Result{
			Check: check, Status: Warn, Message: "no node could be probed" + note,
			Fix: "pass --substrate-node-selector with the labels of the worker nodes, and make sure they are Ready",
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("/var/lib/ate has a filesystem of its own on %d node(s)%s", probed, note)}
}
