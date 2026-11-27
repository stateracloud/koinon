// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"errors"
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

type fakeAteFS struct {
	nodes []preflight.NodeAteFS
	err   error
}

func (f fakeAteFS) ProbeAteFS(context.Context) ([]preflight.NodeAteFS, error) { return f.nodes, f.err }

var _ = Describe("Substrate /var/lib/ate filesystem check", func() {
	const check = "Substrate /var/lib/ate filesystem"
	cluster := func(nodes ...preflight.NodeAteFS) *substrateCluster {
		c := healthySubstrate()
		c.atefs = fakeAteFS{nodes: nodes}
		return c
	}

	It("passes when every node has a filesystem of its own for /var/lib/ate", func(ctx SpecContext) {
		r := find(cluster(preflight.NodeAteFS{Node: "a"}, preflight.NodeAteFS{Node: "b"}).run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Pass), r.Message)
	})

	It("warns, and does not fail, on a node that shares the root filesystem, and names the fix and the missing cap", func(ctx SpecContext) {
		r := find(cluster(preflight.NodeAteFS{Node: "a"}, preflight.NodeAteFS{Node: "w2", Shared: []string{"root", "kubelet"}}).run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(And(ContainSubstring("w2"), ContainSubstring("w2 (with root and kubelet)"), ContainSubstring("same filesystem as the node root or the kubelet filesystem"), ContainSubstring("0 bytes")))
		Expect(r.Message).NotTo(ContainSubstring("a,"))
		Expect(r.Fix).To(And(ContainSubstring("own filesystem"), ContainSubstring("no hard cap per home"), ContainSubstring("#1495"), ContainSubstring("substrate-install.md")))
	})

	It("warns with the cause when the probe cannot run or a node does not report, and notes skipped nodes", func(ctx SpecContext) {
		c := healthySubstrate()
		c.atefs = fakeAteFS{err: errors.New(`create probe pod: namespaces "ate-system" not found`)}
		r := find(c.run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(ContainSubstring("not found"))

		r = find(cluster(preflight.NodeAteFS{Node: "a"}, preflight.NodeAteFS{Node: "b", Err: errors.New("FailedMount")}).run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(And(ContainSubstring("1 of 2"), ContainSubstring("b: FailedMount")))

		r = find(cluster(preflight.NodeAteFS{Node: "a"}, preflight.NodeAteFS{Node: "new", Missing: true}).run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(ContainSubstring("does not exist yet on new"))

		r = find(cluster(preflight.NodeAteFS{Node: "w"}, preflight.NodeAteFS{Node: "cp", Skipped: "cordoned"}).run(ctx), check)
		Expect(r.Status).To(Equal(preflight.Pass))
		Expect(r.Message).To(And(ContainSubstring("1 node(s)"), ContainSubstring("not probed: cp (cordoned)")))

		Expect(find(cluster(preflight.NodeAteFS{Node: "cp", Skipped: "cordoned"}).run(ctx), check).Status).To(Equal(preflight.Warn))
	})

	It("warns when the probe is disabled", func(ctx SpecContext) {
		c := healthySubstrate()
		c.atefs = nil
		Expect(find(c.run(ctx), check).Status).To(Equal(preflight.Warn))
	})

	It("reads the probe line", func() {
		shared, missing, err := preflight.ParseAteFSOutput("noise\n" + preflight.AteFSLine([]string{"root", "kubelet"}, false) + "\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(shared).To(Equal([]string{"root", "kubelet"}))
		Expect(missing).To(BeFalse())
		shared, missing, err = preflight.ParseAteFSOutput(preflight.AteFSLine(nil, false))
		Expect(err).NotTo(HaveOccurred())
		Expect(shared).To(BeEmpty())
		Expect(missing).To(BeFalse())
		_, missing, err = preflight.ParseAteFSOutput(preflight.AteFSLine(nil, true))
		Expect(err).NotTo(HaveOccurred())
		Expect(missing).To(BeTrue())
		_, _, err = preflight.ParseAteFSOutput("nothing\n")
		Expect(err).To(HaveOccurred())
		_, _, err = preflight.ParseAteFSOutput(preflight.SubstrateAteFSProbeMarker + " shared=maybe\n")
		Expect(err).To(HaveOccurred())
	})

	Describe("the probe pods", func() {
		node := func(name string) *corev1.Node {
			return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": "workers"}}, Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			}}
		}

		It("runs one pod on each selected node and reports each answer", func(ctx SpecContext) {
			client := fake.NewClientset(node("a"), node("b"))
			client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
				pod.Name = "probe-" + pod.Spec.NodeName
				pod.Status.Phase = corev1.PodSucceeded
				return false, nil, nil
			})
			logs := map[string]string{"probe-a": preflight.AteFSLine(nil, false) + "\n", "probe-b": preflight.AteFSLine([]string{"root"}, false) + "\n"}
			p := preflight.KubeSubstrateProber{
				Client: client, Namespace: "ate-system", NamePrefix: "x", Image: "img", NodeSelector: map[string]string{"pool": "workers"},
				Timeout: time.Second, PollInterval: time.Millisecond,
				Logs: func(_ context.Context, _, name string) (string, error) { return logs[name], nil },
			}
			got, err := p.ProbeAteFS(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]preflight.NodeAteFS{{Node: "a"}, {Node: "b", Shared: []string{"root"}}}))
			left, err := client.CoreV1().Pods("ate-system").List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(left.Items).To(BeEmpty(), "the probe pods are deleted")

			p.NodeSelector = map[string]string{"pool": "none"}
			_, err = p.ProbeAteFS(ctx)
			Expect(err).To(MatchError(ContainSubstring("no node matches")))
		})

		It("is a read-only pod that mounts only the node root, read-only, bound to the node", func() {
			pod := preflight.KubeSubstrateProber{Namespace: "ate-system", NamePrefix: "x", Image: "img"}.AteFSPod("a")
			Expect(pod.Spec.NodeName).To(Equal("a"))
			Expect(pod.Spec.Tolerations).To(ConsistOf(corev1.Toleration{Operator: corev1.TolerationOpExists}))
			paths := map[string]string{}
			for _, v := range pod.Spec.Volumes {
				paths[v.Name] = v.HostPath.Path
			}
			Expect(paths).To(Equal(map[string]string{"root": "/"}))
			for _, m := range pod.Spec.Containers[0].VolumeMounts {
				Expect(m.ReadOnly).To(BeTrue(), m.Name)
			}
			sc := pod.Spec.Containers[0].SecurityContext
			Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(*sc.Privileged).To(BeFalse())
		})
	})
})
