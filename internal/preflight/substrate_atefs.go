// SPDX-License-Identifier: Apache-2.0

package preflight

// The filesystem check of /var/lib/ate (issue #1494). The home of a sandbox is a file or a directory under
// /var/lib/ate/actors/<uid> on the node, in every tier. Kubelet does not see that hostPath. A home that fills up
// fills the filesystem that holds /var/lib/ate, and kubelet reports 0 bytes used. v0.1 has no hard cap per home, so
// the protection of the node is a filesystem of its own for /var/lib/ate. The check warns when /var/lib/ate is on
// the same filesystem as the node root or as /var/lib/kubelet. It reads the device numbers from a short pod on each node.
// The pod mounts only the node root; the container runtime binds it recursively, so the mounts below it are visible.
// Limit: a btrfs subvolume or a ZFS dataset has its own device number and shares the space. The check cannot see that.

import (
	"context"
	"fmt"
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
	// ateFSProbeRoot is the mount point of the node root in the probe pod. The paths below it are the node's paths.
	ateFSProbeRoot = "/probe/root"
	// AteFSMissing is the probe answer of a node that has no AteDir yet (no worker ran there).
	AteFSMissing = "missing"
	// AteFSSeparate is the probe answer of a node where AteDir shares no filesystem with the root or kubelet.
	AteFSSeparate = "none"
)

// NodeAteFS is what the probe pod of one node saw.
type NodeAteFS struct {
	Node string
	// Shared names what AteDir shares a filesystem with: "root" (the node root), "kubelet" (/var/lib/kubelet), or both.
	// It is empty when AteDir has a filesystem of its own.
	Shared []string
	// Missing is true when the node has no AteDir yet.
	Missing bool
	// Skipped is why the node was not probed (see skipReason).
	Skipped string
	// Err is why the node could not be probed.
	Err error
}

// AteFSProber finds out, on each node, whether AteDir has a filesystem of its own.
type AteFSProber interface {
	ProbeAteFS(ctx context.Context) ([]NodeAteFS, error)
}

// AteFSLine formats the probe line: shared is the list of what AteDir shares a filesystem with, or empty.
// missing is true when AteDir does not exist.
func AteFSLine(shared []string, missing bool) string {
	v := AteFSSeparate
	switch {
	case missing:
		v = AteFSMissing
	case len(shared) > 0:
		v = strings.Join(shared, ",")
	}
	return fmt.Sprintf("%s shared=%s", SubstrateAteFSProbeMarker, v)
}

// ParseAteFSOutput reads the probe line from the probe pod's log.
func ParseAteFSOutput(log string) (shared []string, missing bool, err error) {
	for _, line := range strings.Split(log, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), SubstrateAteFSProbeMarker+" shared=")
		if !ok {
			continue
		}
		switch rest {
		case AteFSSeparate:
			return nil, false, nil
		case AteFSMissing:
			return nil, true, nil
		}
		for _, w := range strings.Split(rest, ",") {
			if w != "root" && w != "kubelet" {
				return nil, false, fmt.Errorf("malformed probe output %q", truncate(line, 200))
			}
			shared = append(shared, w)
		}
		return shared, false, nil
	}
	return nil, false, fmt.Errorf("no %q line in the probe pod's log: %q", SubstrateAteFSProbeMarker, truncate(log, 200))
}

// AteFSPod is the probe pod of one node. It mounts the node root read-only and compares the device numbers of
// /var/lib/ate, /var/lib/kubelet and the root below it. It names the node and tolerates every taint, as the
// user-namespace probe does. It mounts only "/" (a path that always exists), so a node without /var/lib/ate gets
// an answer ("missing") and not a pod that waits for a mount.
func (p KubeSubstrateProber) AteFSPod(node string) *corev1.Pod {
	volumes := []corev1.Volume{{Name: "root", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
		Path: "/", Type: ptr.To(corev1.HostPathDirectory),
	}}}}
	mounts := []corev1.VolumeMount{{Name: "root", MountPath: ateFSProbeRoot, ReadOnly: true}}
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
			res.Shared, res.Missing, err = ParseAteFSOutput(log)
		}
		res.Err = err
		out = append(out, res)
	}
	return out, nil
}

// AteFSFix is the Fix text of the warning.
const AteFSFix = "Put /var/lib/ate on its own filesystem on each worker node: a separate disk or partition that is mounted before kubelet starts. " +
	"Then a full sandbox home fills that filesystem and not the root or the kubelet filesystem of the node. " +
	"You can also mount /var/lib/ate/snapshots on its own disk. v0.1 has no hard cap per home (issue #1495). " +
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
	var shared, missing, broken, skipped []string
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
		case n.Missing:
			missing = append(missing, n.Node)
		case len(n.Shared) > 0:
			shared = append(shared, n.Node+" (with "+strings.Join(n.Shared, " and ")+")")
		}
	}
	note := ""
	if len(skipped) > 0 {
		note = "; not probed: " + strings.Join(skipped, ", ")
	}
	if probed == 0 {
		return Result{
			Check: check, Status: Warn, Message: "no node could be probed" + note,
			Fix: "pass --substrate-node-selector with the labels of the worker nodes, and make sure they are Ready",
		}
	}
	var parts, fixes []string
	if len(shared) > 0 {
		parts = append(parts, fmt.Sprintf("/var/lib/ate is on the same filesystem as the node root or the kubelet filesystem on %s: "+
			"a sandbox home that becomes full fills that filesystem, and kubelet reports 0 bytes used for it", strings.Join(shared, ", ")))
		fixes = append(fixes, AteFSFix)
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("/var/lib/ate does not exist yet on %s, so its filesystem is not checked", strings.Join(missing, ", ")))
		fixes = append(fixes, "Mount a filesystem at /var/lib/ate on the nodes where it does not exist before the first worker starts. Then run the check again.")
	}
	if len(broken) > 0 {
		parts = append(parts, fmt.Sprintf("the /var/lib/ate probe pod did not report on %d of %d nodes: %s", len(broken), probed, strings.Join(broken, "; ")))
		fixes = append(fixes, "Fix the cause above. Then run the check again.")
	}
	if len(parts) > 0 {
		return Result{Check: check, Status: Warn, Message: strings.Join(parts, "; ") + note, Fix: strings.Join(fixes, " ")}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("/var/lib/ate has a filesystem of its own on %d node(s)%s", probed, note)}
}
