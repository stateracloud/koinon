// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/preflight"
)

type fakeUserNS struct {
	nodes []preflight.NodeUserNamespaces
	err   error
}

func (f fakeUserNS) ProbeUserNamespaces(context.Context) ([]preflight.NodeUserNamespaces, error) {
	return f.nodes, f.err
}

var _ = Describe("Substrate gVisor user-namespace check", func() {
	const check = "Substrate gVisor user namespaces"
	gvisor := func(nodes ...preflight.NodeUserNamespaces) *substrateCluster {
		c := healthySubstrate()
		c.config.Substrate.GVisor = true
		c.userns = fakeUserNS{nodes: nodes}
		return c
	}

	It("passes when every node allows user namespaces", func(ctx SpecContext) {
		c := gvisor(preflight.NodeUserNamespaces{Node: "a", Max: 11255}, preflight.NodeUserNamespaces{Node: "b", Max: 63000})
		r := find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Pass), r.Message)
	})

	It("fails on a node with 0 and tells the cause, the Talos patch, the sysctl form and the cost", func(ctx SpecContext) {
		c := gvisor(preflight.NodeUserNamespaces{Node: "a", Max: 11255}, preflight.NodeUserNamespaces{Node: "talos-w2", Max: 0})
		r := find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(And(ContainSubstring("talos-w2"), ContainSubstring("user namespace"), ContainSubstring("no space left on device")))
		Expect(r.Message).NotTo(ContainSubstring("a,"))
		Expect(r.Fix).To(And(
			ContainSubstring("machine.sysctls"), ContainSubstring("user.max_user_namespaces"), ContainSubstring("11255"),
			ContainSubstring("sysctl -w"), ContainSubstring("attack surface"),
		))
	})

	It("passes with the skipped control plane named, and warns when no node could be probed", func(ctx SpecContext) {
		c := gvisor(preflight.NodeUserNamespaces{Node: "w", Max: 11255}, preflight.NodeUserNamespaces{Node: "cp", Skipped: "tainted node-role.kubernetes.io/control-plane"})
		r := find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Pass))
		Expect(r.Message).To(And(ContainSubstring("1 node(s)"), ContainSubstring("not probed: cp (tainted")))
		c = gvisor(preflight.NodeUserNamespaces{Node: "cp", Skipped: "cordoned"})
		Expect(find(c.run(ctx), check).Status).To(Equal(preflight.Warn))
	})

	It("skips with a clear line when the gVisor class is not in use, and does not probe", func(ctx SpecContext) {
		c := gvisor(preflight.NodeUserNamespaces{Node: "a", Max: 0})
		c.config.Substrate.GVisor = false
		r := find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Pass))
		Expect(r.Message).To(ContainSubstring("not checked: the gVisor sandbox class is not in use"))
	})

	It("fails with the real cause, not a sysctl finding, when the probe cannot run or a node does not report", func(ctx SpecContext) {
		c := healthySubstrate()
		c.config.Substrate.GVisor = true
		c.userns = fakeUserNS{err: errors.New(`create probe pod: namespaces "ate-system" not found`)}
		r := find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring("not found"))
		Expect(r.Fix).NotTo(ContainSubstring("machine.sysctls"))

		c.userns = fakeUserNS{nodes: []preflight.NodeUserNamespaces{{Node: "a", Max: 11255}, {Node: "b", Err: errors.New("still Pending")}}}
		r = find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(And(ContainSubstring("1 of 2"), ContainSubstring("b: still Pending")))
		Expect(r.Fix).NotTo(ContainSubstring("machine.sysctls"))
	})

	It("warns when the probe is disabled", func(ctx SpecContext) {
		c := healthySubstrate()
		c.config.Substrate.GVisor = true
		Expect(find(c.run(ctx), check).Status).To(Equal(preflight.Warn))
	})

	It("reads the sysctl and the probe line", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "max_user_namespaces")
		Expect(os.WriteFile(path, []byte("11255\n"), 0o600)).To(Succeed())
		Expect(preflight.ReadMaxUserNamespaces(path)).To(BeEquivalentTo(11255))
		// A kernel without user namespaces has no file: the same as 0.
		Expect(preflight.ReadMaxUserNamespaces(filepath.Join(dir, "missing"))).To(BeEquivalentTo(0))
		Expect(os.WriteFile(path, []byte("many"), 0o600)).To(Succeed())
		_, err := preflight.ReadMaxUserNamespaces(path)
		Expect(err).To(HaveOccurred())

		Expect(preflight.ParseUserNamespacesOutput("noise\n" + preflight.UserNamespacesLine(0) + "\n")).To(BeEquivalentTo(0))
		_, err = preflight.ParseUserNamespacesOutput("nothing\n")
		Expect(err).To(HaveOccurred())
		_, err = preflight.ParseUserNamespacesOutput(preflight.SubstrateUserNSProbeMarker + " max=x\n")
		Expect(err).To(HaveOccurred())
	})

	Describe("the probe pods", func() {
		node := func(name string, lbl map[string]string) *corev1.Node {
			return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbl}, Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			}}
		}
		prober := func(client *fake.Clientset, logs map[string]string) preflight.KubeSubstrateProber {
			client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
				pod.Name = "probe-" + pod.Spec.NodeName
				pod.Status.Phase = corev1.PodSucceeded
				return false, nil, nil
			})
			return preflight.KubeSubstrateProber{
				Client: client, Namespace: "ate-system", NamePrefix: "x", Image: "img", NodeSelector: map[string]string{"pool": "workers"},
				Labels: map[string]string{"app.kubernetes.io/part-of": "koinon"}, Timeout: time.Second, PollInterval: time.Millisecond,
				Logs: func(_ context.Context, _, name string) (string, error) {
					return logs[name], nil
				},
			}
		}

		It("runs one pod on each selected node and reports each value", func(ctx SpecContext) {
			client := fake.NewClientset(node("a", map[string]string{"pool": "workers"}), node("b", map[string]string{"pool": "workers"}), node("cp", nil))
			p := prober(client, map[string]string{
				"probe-a": preflight.UserNamespacesLine(11255) + "\n",
				"probe-b": preflight.UserNamespacesLine(0) + "\n",
			})
			got, err := p.ProbeUserNamespaces(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]preflight.NodeUserNamespaces{{Node: "a", Max: 11255}, {Node: "b", Max: 0}}))
			left, err := client.CoreV1().Pods("ate-system").List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(left.Items).To(BeEmpty(), "the probe pods are deleted")
		})

		It("leaves out the control plane, cordoned and not-Ready nodes without a selector, and probes a selected tainted node", func(ctx SpecContext) {
			cp := node("cp", nil)
			cp.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
			cordoned := node("cordoned", nil)
			cordoned.Spec.Unschedulable = true
			down := node("down", nil)
			down.Status.Conditions[0].Status = corev1.ConditionFalse
			client := fake.NewClientset(node("w", nil), cp, cordoned, down)
			p := prober(client, map[string]string{"probe-w": preflight.UserNamespacesLine(11255) + "\n", "probe-cp": preflight.UserNamespacesLine(0) + "\n"})
			p.NodeSelector = nil
			got, err := p.ProbeUserNamespaces(ctx)
			Expect(err).NotTo(HaveOccurred())
			skipped := map[string]string{}
			for _, n := range got {
				skipped[n.Node] = n.Skipped
			}
			Expect(skipped).To(Equal(map[string]string{"w": "", "cp": "tainted node-role.kubernetes.io/control-plane", "cordoned": "cordoned", "down": "not Ready"}))

			// The operator names the workers by label: a tainted node in the selection is probed, a down one is not.
			cp.Labels, down.Labels = map[string]string{"pool": "workers"}, map[string]string{"pool": "workers"}
			client = fake.NewClientset(cp, down)
			p = prober(client, map[string]string{"probe-cp": preflight.UserNamespacesLine(0) + "\n"})
			got, err = p.ProbeUserNamespaces(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(preflight.NodeUserNamespaces{Node: "cp"}, preflight.NodeUserNamespaces{Node: "down", Skipped: "not Ready"}))
		})

		It("keeps the error of a node whose log has no probe line, and fails when no node matches", func(ctx SpecContext) {
			client := fake.NewClientset(node("a", map[string]string{"pool": "workers"}))
			p := prober(client, map[string]string{"probe-a": "oops\n"})
			got, err := p.ProbeUserNamespaces(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1))
			Expect(got[0].Err).To(MatchError(ContainSubstring(preflight.SubstrateUserNSProbeMarker)))

			p.NodeSelector = map[string]string{"pool": "none"}
			_, err = p.ProbeUserNamespaces(ctx)
			Expect(err).To(MatchError(ContainSubstring("no node matches")))
		})

		It("is a read-only pod without privileges, bound to the node and tolerating its taints", func() {
			pod := preflight.KubeSubstrateProber{Namespace: "ate-system", NamePrefix: "x", Image: "img"}.UserNamespacesPod("a")
			Expect(pod.Spec.NodeName).To(Equal("a"))
			Expect(pod.Spec.Volumes).To(BeEmpty())
			Expect(pod.Spec.Tolerations).To(ConsistOf(corev1.Toleration{Operator: corev1.TolerationOpExists}))
			sc := pod.Spec.Containers[0].SecurityContext
			Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(*sc.Privileged).To(BeFalse())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(*pod.Spec.ActiveDeadlineSeconds).To(BeNumerically(">", 0))
		})
	})
})
