// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/preflight"
)

var _ = Describe("ParseNetPolOutput", func() {
	DescribeTable("round-trips what the client prints",
		func(res preflight.NetPolResult) {
			Expect(preflight.ParseNetPolOutput("starting\n" + res.String() + "\n")).To(Equal(res))
		},
		Entry("enforced", preflight.NetPolResult{ControlReached: true}),
		Entry("not enforced", preflight.NetPolResult{ControlReached: true, DeniedReached: true}),
		Entry("inconclusive", preflight.NetPolResult{}),
	)

	It("rejects a log without a probe line", func() {
		_, err := preflight.ParseNetPolOutput("exec format error")
		Expect(err).To(MatchError(ContainSubstring(`no "koinon-netpol-probe" line`)))
	})

	It("rejects a malformed probe line", func() {
		_, err := preflight.ParseNetPolOutput("koinon-netpol-probe control=ok")
		Expect(err).To(MatchError(ContainSubstring("malformed")))
	})
})

var _ = Describe("NetPolClient", func() {
	// freePort returns a local port nothing listens on.
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		port := l.Addr().(*net.TCPAddr).Port
		Expect(l.Close()).To(Succeed())
		return port
	}
	// serve opens n probe ports and answers on them until the spec ends. The ports are open when
	// serve returns, so a client never races the server's start, and a port is never picked and
	// then lost to another process.
	serve := func(n int) []int {
		listeners := make([]net.Listener, n)
		ports := make([]int, n)
		for i := range listeners {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			Expect(err).NotTo(HaveOccurred())
			listeners[i], ports[i] = l, l.Addr().(*net.TCPAddr).Port
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- preflight.ServeNetPolProbeOn(ctx, listeners...) }()
		DeferCleanup(func() {
			cancel()
			Eventually(done).Should(Receive(BeNil()))
		})
		return ports
	}
	// client's Attempt is generous: an answering port replies at once, a blocked one is refused
	// at once, so the bound only matters on a stalled host, where a short one would read a slow
	// answer as a blocked port.
	client := func(control, denied int) preflight.NetPolClient {
		return preflight.NetPolClient{
			Host: "127.0.0.1", ControlPort: control, DeniedPort: denied,
			ControlWait: 300 * time.Millisecond, Window: 200 * time.Millisecond,
			Attempt: 5 * time.Second, Pause: 20 * time.Millisecond,
		}
	}

	It("reports the denied port open when it answers for the whole window", func(ctx SpecContext) {
		ports := serve(2)
		Expect(client(ports[0], ports[1]).Run(ctx)).To(Equal(preflight.NetPolResult{ControlReached: true, DeniedReached: true}))
	})

	It("reports the denied port blocked when a connection to it fails", func(ctx SpecContext) {
		control, denied := serve(1)[0], freePort()
		Expect(client(control, denied).Run(ctx)).To(Equal(preflight.NetPolResult{ControlReached: true}))
	})

	It("waits for the control port, then gives up", func(ctx SpecContext) {
		start := time.Now()
		Expect(client(freePort(), freePort()).Run(ctx)).To(Equal(preflight.NetPolResult{}))
		Expect(time.Since(start)).To(BeNumerically(">=", 300*time.Millisecond))
	})

	It("does not count an answer that is not the probe server's", func(ctx SpecContext) {
		// Something else on the control port, such as a proxy's error page, is not the server.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = c.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 4\r\n\r\nnope"))
				_ = c.Close()
			}
		}()
		DeferCleanup(l.Close)
		port := l.Addr().(*net.TCPAddr).Port
		Expect(client(port, port).Run(ctx).ControlReached).To(BeFalse())
	})
})

var _ = Describe("KubeNetPolProber", func() {
	const (
		ns       = "koinon"
		server   = "koinon-preflight-netpol-server-t0k3n"
		clientPd = "koinon-preflight-netpol-client-t0k3n"
		policy   = "koinon-preflight-netpol-t0k3n"
		serverIP = "10.244.0.9"
	)
	var (
		kube    *fake.Clientset
		created map[string]runtime.Object
	)

	// startPods gives created pods a status: the server runs (or stays Pending), the client
	// finishes (or keeps running).
	startPods := func(serverPhase, clientPhase corev1.PodPhase) {
		kube.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj := a.(k8stesting.CreateAction).GetObject()
			created[obj.(metav1.Object).GetName()] = obj.DeepCopyObject()
			if pod, ok := obj.(*corev1.Pod); ok {
				pod.UID = types.UID("uid-" + pod.Name)
				switch pod.Name {
				case server:
					pod.Status.Phase = serverPhase
					if serverPhase == corev1.PodRunning {
						pod.Status.PodIP = serverIP
					}
				case clientPd:
					pod.Status.Phase = clientPhase
				}
			}
			return false, nil, nil
		})
	}

	prober := func(log string) preflight.KubeNetPolProber {
		return preflight.KubeNetPolProber{
			Client: kube, Namespace: ns, NamePrefix: "koinon-preflight-netpol", Token: "t0k3n",
			Image: "ghcr.io/stateracloud/koinon/api:0.1.0", ImagePullSecrets: []string{"ghcr"},
			Labels:  map[string]string{"app.kubernetes.io/instance": "koinon"},
			Timeout: 300 * time.Millisecond, PollInterval: 10 * time.Millisecond,
			Logs: func(_ context.Context, namespace, name string) (string, error) {
				Expect(namespace + "/" + name).To(Equal(ns + "/" + clientPd))
				return log, nil
			},
		}
	}

	expectCleanedUp := func(ctx context.Context) {
		pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(BeEmpty(), "the probe pods are deleted")
		policies, err := kube.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(policies.Items).To(BeEmpty(), "the probe NetworkPolicy is deleted")
	}

	BeforeEach(func() {
		kube = fake.NewClientset()
		created = map[string]runtime.Object{}
	})

	It("passes the check when the policy is enforced, and deletes all three objects", func(ctx SpecContext) {
		startPods(corev1.PodRunning, corev1.PodSucceeded)
		p := prober("koinon-netpol-probe control=ok denied=blocked\n")
		res, err := p.Probe(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(preflight.NetPolResult{ControlReached: true}))
		Expect(preflight.CheckNetworkPolicy(&preflight.Checker{NetPol: p}, ctx).Status).To(Equal(preflight.Pass))
		expectCleanedUp(ctx)

		// The server: Koinon's image, the default runtime, restricted, labelled for the policy.
		srv := created[server].(*corev1.Pod)
		Expect(srv.Namespace).To(Equal(ns))
		Expect(srv.Spec.RuntimeClassName).To(BeNil())
		Expect(*srv.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(srv.Spec.ImagePullSecrets).To(Equal([]corev1.LocalObjectReference{{Name: "ghcr"}}))
		Expect(*srv.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(srv.Labels).To(HaveKeyWithValue("koinon.dev/preflight-netpol-probe", "t0k3n"))
		Expect(srv.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "preflight-netpol-server"))
		Expect(srv.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "koinon"))
		ctr := srv.Spec.Containers[0]
		Expect(ctr.Image).To(Equal("ghcr.io/stateracloud/koinon/api:0.1.0"))
		Expect(ctr.Args).To(Equal([]string{"preflight", "netpol-server"}))
		Expect(*ctr.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*ctr.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(ctr.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
		Expect(ctr.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		Expect(ctr.Resources.Limits.Cpu().IsZero()).To(BeFalse())
		Expect(ctr.Resources.Limits.Memory().IsZero()).To(BeFalse())

		// The policy: selects only the server and allows ingress only on the control port.
		np := created[policy].(*networkingv1.NetworkPolicy)
		Expect(np.Spec.PodSelector.MatchLabels).To(Equal(map[string]string{"koinon.dev/preflight-netpol-probe": "t0k3n"}))
		Expect(np.Spec.PolicyTypes).To(Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}))
		Expect(np.Spec.Ingress).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].From).To(BeEmpty())
		Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(preflight.NetPolControlPort))
		Expect(np.OwnerReferences).To(ConsistOf(HaveField("Name", server)))

		// The client: connects to the server's IP and is owned by the server.
		cl := created[clientPd].(*corev1.Pod)
		Expect(cl.Spec.Containers[0].Args).To(ContainElements("netpol-client", "--target="+serverIP))
		Expect(cl.Labels).NotTo(HaveKey("koinon.dev/preflight-netpol-probe"), "the policy does not select the client")
		Expect(cl.OwnerReferences).To(ConsistOf(And(HaveField("Name", server), HaveField("UID", types.UID("uid-"+server)))))
	})

	It("warns when the policy is not enforced", func(ctx SpecContext) {
		startPods(corev1.PodRunning, corev1.PodSucceeded)
		p := prober("koinon-netpol-probe control=ok denied=open\n")
		r := preflight.CheckNetworkPolicy(&preflight.Checker{NetPol: p}, ctx)
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(Equal(preflight.NetPolNotEnforced))
		expectCleanedUp(ctx)
	})

	It("cleans up and reports why when the server never runs", func(ctx SpecContext) {
		startPods(corev1.PodPending, corev1.PodSucceeded)
		kube.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			pod := created[a.(k8stesting.GetAction).GetName()].(*corev1.Pod).DeepCopy()
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "probe", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull denied"}},
			}}
			return true, pod, nil
		})
		_, err := prober("").Probe(ctx)
		Expect(err).To(MatchError(And(
			ContainSubstring("probe pod "+server+" still Pending"),
			ContainSubstring("ImagePullBackOff: pull denied"),
		)))
		Expect(created).NotTo(HaveKey(clientPd), "no client without a running server")
		expectCleanedUp(ctx)

		r := preflight.CheckNetworkPolicy(&preflight.Checker{NetPol: prober("")}, ctx)
		Expect(r.Status).To(Equal(preflight.Warn), "a timeout warns, never fails")
	})

	It("cleans up when the client never finishes", func(ctx SpecContext) {
		startPods(corev1.PodRunning, corev1.PodRunning)
		_, err := prober("").Probe(ctx)
		Expect(err).To(MatchError(ContainSubstring("probe pod " + clientPd + " still Running")))
		expectCleanedUp(ctx)
	})
})
