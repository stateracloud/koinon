// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The chart template that renders the worker-namespace policies of the Substrate driver
// (ADR 0120 section 6, issue #1089). Until #1089 adds it, the specs below skip, and fail under
// KOINON_REQUIRE=substrate-policy.
const workerPolicyTemplate = "templates/substrate-policy.yaml"

// The worker pods' namespace (the chart creates it apart from the control plane), the control-plane
// namespace, and the identities of the rules. The defaults are Substrate's own and the chart's; set the
// variables when the chart names them otherwise.
var (
	workerNamespace  = envOr("KOINON_ESCAPE_WORKER_NAMESPACE", "ate-workers")
	controlNamespace = envOr("KOINON_ESCAPE_CONTROL_NAMESPACE", "ate-system")
	workerController = envOr("KOINON_ESCAPE_WORKER_CONTROLLER", "system:serviceaccount:ate-system:ate-controller")
	// The only identity that creates worker pods (from a ReplicaSet), and the one that creates the node agent's.
	replicaSetController = "system:serviceaccount:kube-system:replicaset-controller"
	daemonSetController  = "system:serviceaccount:kube-system:daemon-set-controller"
	// A hostPath of the documented set (ADR 0120 section 6.3): a worker needs the KVM device.
	workerHostPathOK = envOr("KOINON_ESCAPE_WORKER_HOSTPATH", "/dev/kvm")
)

// The digest of an image that exists only in these specs.
const workerDigest = "registry.invalid/koinon-escape/worker@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// The ValidatingAdmissionPolicies of the Substrate driver's two namespaces (ADR 0120 section 6),
// as `helm template` renders them, against a real API server (envtest): worker pods only from
// the ReplicaSet controller, for a ReplicaSet; image digests pinned; no hostNetwork; no hostPath outside the documented
// set; no network volume, no service account token, a capability ceiling; no ephemeral containers and no exec into a
// worker; and in the control-plane namespace a hostPath only for the node agent (atelet). Pod Security admission is not enforced in
// envtest, so every denial is the policy's. These specs do not depend on the driver, but on the chart:
// they skip while the chart renders no worker policy.
var _ = Describe("Worker pod admission policy", Label("escape", "admission", "substrate"), Ordered, ContinueOnFailure, func() {
	var (
		admin  *kubernetes.Clientset
		as     map[string]*kubernetes.Clientset
		dryRun = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	)
	const (
		other = "system:serviceaccount:koinon:koinon-reconciler"
		// What kubeadm and kind give their admin: not system:masters.
		kubeadmAdmin = "kubernetes-admin"
		existing     = "existing-worker"
	)

	isTrue := true
	worker := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "11111111-1111-1111-1111-111111111111", Controller: &isTrue,
			}}},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: workerDigest}},
				Volumes: []corev1.Volume{{Name: "kvm", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: workerHostPathOK},
				}}},
			},
		}
		for _, m := range mutate {
			m(p)
		}
		return p
	}
	create := func(ctx context.Context, user, namespace string, p *corev1.Pod) error {
		_, err := as[user].CoreV1().Pods(namespace).Create(ctx, p, dryRun)
		return err
	}
	denied := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial by a ValidatingAdmissionPolicy")
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings := renderWorkerPolicy(ctx)

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		for _, user := range []string{workerController, replicaSetController, daemonSetController, other, kubeadmAdmin} {
			// Full RBAC rights, so that only admission can say no.
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-worker-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			c := rest.CopyConfig(cfg)
			groups := []string{"system:serviceaccounts", "system:authenticated"}
			if parts := strings.Split(user, ":"); len(parts) == 4 && parts[1] == "serviceaccount" {
				// A real token also carries its namespace's group.
				groups = append(groups, "system:serviceaccounts:"+parts[2])
			}
			if user == kubeadmAdmin {
				groups = []string{"kubeadm:cluster-admins", "system:authenticated"}
			}
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
			as[user] = kubernetes.NewForConfigOrDie(c)
		}
		for _, ns := range []string{workerNamespace, controlNamespace} {
			_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: ns, Labels: map[string]string{"kubernetes.io/metadata.name": ns},
			}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = admin.CoreV1().ServiceAccounts(ns).Create(ctx,
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = admin.CoreV1().ServiceAccounts(ns).Create(ctx,
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "atelet"}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		// The pod the update and connect cases act on exists before the policies do.
		_, err = admin.CoreV1().Pods(workerNamespace).Create(ctx, worker(existing), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		for _, p := range policies {
			_, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, p, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, b := range bindings {
			_, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, b, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("waiting until the policies are enforced")
		// A pod that breaks the hostNetwork and image rules, so that the wait does not depend on one of them.
		Eventually(func() error {
			return create(ctx, "admin", workerNamespace, worker("probe", func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = "registry.invalid/koinon-escape/worker:latest"
			}))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
		Eventually(func() error {
			return create(ctx, "admin", controlNamespace, worker("probe-control", func(p *corev1.Pod) { p.Spec.HostNetwork = true }))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("admits a compliant worker pod from the ReplicaSet controller (control)", func(ctx SpecContext) {
		Expect(create(ctx, replicaSetController, workerNamespace, worker("ok"))).To(Succeed())
	})

	It("denies a worker pod from any other identity, the Substrate controller included", func(ctx SpecContext) {
		for _, user := range []string{"admin", other, workerController, daemonSetController} {
			denied(create(ctx, user, workerNamespace, worker("by-"+strings.ToLower(strings.NewReplacer(":", "-", "_", "-").Replace(user)))))
		}
	})

	It("denies a worker pod that no ReplicaSet owns", func(ctx SpecContext) {
		denied(create(ctx, replicaSetController, workerNamespace, worker("no-owner", func(p *corev1.Pod) { p.OwnerReferences = nil })))
		denied(create(ctx, replicaSetController, workerNamespace, worker("job-owner", func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Job" })))
		denied(create(ctx, replicaSetController, workerNamespace, worker("not-controller", func(p *corev1.Pod) { p.OwnerReferences[0].Controller = nil })))
	})

	It("denies an image that is not pinned by digest", func(ctx SpecContext) {
		for _, image := range []string{"registry.invalid/koinon-escape/worker:v1", "registry.invalid/koinon-escape/worker:latest", "registry.invalid/koinon-escape/worker"} {
			denied(create(ctx, replicaSetController, workerNamespace, worker("tag", func(p *corev1.Pod) { p.Spec.Containers[0].Image = image })))
		}
		denied(create(ctx, replicaSetController, workerNamespace, worker("init-tag", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "registry.invalid/koinon-escape/init:v1"}}
		})))
	})

	It("denies hostNetwork and a hostPath outside the documented set", func(ctx SpecContext) {
		denied(create(ctx, replicaSetController, workerNamespace, worker("host-network", func(p *corev1.Pod) { p.Spec.HostNetwork = true })))
		for _, path := range []string{"/", "/etc", "/var/run/docker.sock", "/var/lib/kubelet", workerHostPathOK + "x"} {
			denied(create(ctx, replicaSetController, workerNamespace, worker("host-path", func(p *corev1.Pod) {
				p.Spec.Volumes[0].HostPath.Path = path
			})))
			// The documented volume stays, so a policy that reads only the first volume fails here.
			denied(create(ctx, replicaSetController, workerNamespace, worker("host-path-second", func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "extra", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: path},
				}})
			})))
		}
	})

	It("admits the base directory of the worker, /var/lib/ate", func(ctx SpecContext) {
		Expect(create(ctx, replicaSetController, workerNamespace, worker("ate-base", func(p *corev1.Pod) {
			p.Spec.Volumes[0].HostPath.Path = "/var/lib/ate"
		}))).To(Succeed())
	})

	It("admits a pod volume that is hostPath, emptyDir, configMap, downwardAPI or projected without a token", func(ctx SpecContext) {
		vols := []corev1.Volume{
			{Name: "e", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "c", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}}},
			{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
				DownwardAPI: &corev1.DownwardAPIProjection{},
			}}}}},
		}
		Expect(create(ctx, replicaSetController, workerNamespace, worker("safe-volumes", func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, vols...)
		}))).To(Succeed())
	})

	It("denies a volume of another kind (PVC, CSI, NFS, ephemeral) beside the allowed hostPath", func(ctx SpecContext) {
		vols := map[string]corev1.VolumeSource{
			"pvc": {PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}},
			"csi": {CSI: &corev1.CSIVolumeSource{Driver: "example.invalid"}},
			"nfs": {NFS: &corev1.NFSVolumeSource{Server: "nfs.invalid", Path: "/"}},
			"ephemeral": {Ephemeral: &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			}}}},
			"secret": {Secret: &corev1.SecretVolumeSource{SecretName: "s"}},
		}
		for name, src := range vols {
			denied(create(ctx, replicaSetController, workerNamespace, worker("vol-"+name, func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "extra", VolumeSource: src})
			})))
		}
	})

	It("denies a projected service account token, and automount true; unset and false pass", func(ctx SpecContext) {
		// This is what the API server injects into a pod whose ServiceAccount mounts a token (kube-api-access-*).
		token := corev1.Volume{Name: "kube-api-access-x", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}},
		}}}
		denied(create(ctx, replicaSetController, workerNamespace, worker("sa-token", func(p *corev1.Pod) {
			p.Spec.ServiceAccountName = "default"
			p.Spec.Volumes = append(p.Spec.Volumes, token)
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("automount-true", func(p *corev1.Pod) {
			p.Spec.AutomountServiceAccountToken = &isTrue
		})))
		no := false
		Expect(create(ctx, replicaSetController, workerNamespace, worker("automount-false", func(p *corev1.Pod) {
			p.Spec.AutomountServiceAccountToken = &no
		}))).To(Succeed())
		// The pod of a pool leaves the field unset (a "must be false" rule would refuse it).
		Expect(create(ctx, replicaSetController, workerNamespace, worker("automount-unset"))).To(Succeed())
	})

	It("limits added capabilities to the ceiling", func(ctx SpecContext) {
		caps := func(add ...corev1.Capability) func(*corev1.Pod) {
			return func(p *corev1.Pod) {
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: add}}
			}
		}
		Expect(create(ctx, replicaSetController, workerNamespace, worker("caps-ok", caps("SYS_ADMIN", "NET_ADMIN")))).To(Succeed())
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-ptrace", caps("SYS_ADMIN", "SYS_PTRACE"))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-all", caps("ALL"))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-init", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: workerDigest, SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_MODULE"}},
			}}}
		})))
	})

	Context("the control-plane namespace and the node agent", func() {
		atelet := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
			return worker(name, append([]func(*corev1.Pod){func(p *corev1.Pod) {
				p.OwnerReferences[0].Kind = "DaemonSet"
				p.Spec.ServiceAccountName = "atelet"
				p.Spec.Volumes[0].HostPath.Path = "/var/lib/ate"
			}}, mutate...)...)
		}
		It("admits the atelet pod: the DaemonSet controller, a DaemonSet owner, the atelet ServiceAccount, an allowed hostPath (control)", func(ctx SpecContext) {
			Expect(create(ctx, daemonSetController, controlNamespace, atelet("atelet"))).To(Succeed())
		})
		It("denies the same pod from another identity, with another owner or ServiceAccount, or with a hostPath outside its list", func(ctx SpecContext) {
			denied(create(ctx, replicaSetController, controlNamespace, atelet("by-rs")))
			denied(create(ctx, "admin", controlNamespace, atelet("by-admin")))
			denied(create(ctx, daemonSetController, controlNamespace, atelet("wrong-owner", func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "StatefulSet" })))
			denied(create(ctx, daemonSetController, controlNamespace, atelet("no-owner", func(p *corev1.Pod) { p.OwnerReferences = nil })))
			denied(create(ctx, daemonSetController, controlNamespace, atelet("wrong-sa", func(p *corev1.Pod) { p.Spec.ServiceAccountName = "default" })))
			denied(create(ctx, daemonSetController, controlNamespace, atelet("no-sa", func(p *corev1.Pod) { p.Spec.ServiceAccountName = "" })))
			for _, path := range []string{"/", "/etc", "/var/run/docker.sock", "/var/lib/kubelet"} {
				denied(create(ctx, daemonSetController, controlNamespace, atelet("bad-path", func(p *corev1.Pod) { p.Spec.Volumes[0].HostPath.Path = path })))
			}
		})
		It("gives every other pod there no hostPath, and keeps the digest and hostNetwork rules", func(ctx SpecContext) {
			plain := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
				return worker(name, append([]func(*corev1.Pod){func(p *corev1.Pod) { p.Spec.Volumes = nil }}, mutate...)...)
			}
			Expect(create(ctx, replicaSetController, controlNamespace, plain("api"))).To(Succeed())
			denied(create(ctx, replicaSetController, controlNamespace, worker("with-hostpath")))
			denied(create(ctx, replicaSetController, controlNamespace, plain("host-network", func(p *corev1.Pod) { p.Spec.HostNetwork = true })))
			denied(create(ctx, replicaSetController, controlNamespace, plain("tag", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.invalid/koinon-escape/x:v1" })))
		})
	})

	It("lets the controller apply a Deployment, and the installer of kubeadm and kind write one", func(ctx SpecContext) {
		deploy := func(name string) *appsv1.Deployment {
			labels := map[string]string{"app": name}
			return &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: workerDigest}}},
					},
				},
			}
		}
		for _, user := range []string{workerController, kubeadmAdmin, "admin"} {
			_, err := as[user].AppsV1().Deployments(workerNamespace).Create(ctx, deploy("by-"+strings.NewReplacer(":", "-", "_", "-").Replace(user)), dryRun)
			Expect(err).NotTo(HaveOccurred(), user)
		}
		_, err := as[other].AppsV1().Deployments(workerNamespace).Create(ctx, deploy("by-other"), dryRun)
		denied(err)

		// The controller may not write other workload kinds, nor a Deployment with a host-level template.
		hostNet := deploy("host-net")
		hostNet.Spec.Template.Spec.HostNetwork = true
		_, err = as[workerController].AppsV1().Deployments(workerNamespace).Create(ctx, hostNet, dryRun)
		denied(err)
		bad := deploy("bad-path")
		bad.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc"}}}}
		_, err = as[workerController].AppsV1().Deployments(workerNamespace).Create(ctx, bad, dryRun)
		denied(err)
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "by-controller"},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ds"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ds"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: workerDigest}}},
				},
			},
		}
		_, err = as[workerController].AppsV1().DaemonSets(workerNamespace).Create(ctx, ds, dryRun)
		denied(err)
		token := deploy("token")
		token.Spec.Template.Spec.AutomountServiceAccountToken = &isTrue
		_, err = as[workerController].AppsV1().Deployments(workerNamespace).Create(ctx, token, dryRun)
		denied(err)
		// The controller writes Deployments in the worker namespace only, never in the control plane's.
		_, err = as[workerController].AppsV1().Deployments(controlNamespace).Create(ctx, deploy("in-control"), dryRun)
		denied(err)
		// The installer writes the control plane's workloads (the atelet DaemonSet among them).
		_, err = as[kubeadmAdmin].AppsV1().DaemonSets(controlNamespace).Create(ctx, ds, dryRun)
		Expect(err).NotTo(HaveOccurred())
	})

	It("denies ephemeral containers in a worker pod, the controller's included", func(ctx SpecContext) {
		for _, user := range []string{workerController, other, "admin"} {
			p, err := admin.CoreV1().Pods(workerNamespace).Get(ctx, existing, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: workerDigest},
			}}
			_, err = as[user].CoreV1().Pods(workerNamespace).UpdateEphemeralContainers(ctx, existing, p, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			denied(err)
		}
	})

	It("denies the service proxy of both Substrate namespaces, for every identity", func(ctx SpecContext) {
		// A service proxy reaches a Service port through the API server, past the NetworkPolicy.
		// nodes/proxy is cluster-scoped and outside this namespaced policy.
		for _, ns := range []string{workerNamespace, controlNamespace} {
			for _, user := range []string{"admin", workerController, other} {
				req := as[user].CoreV1().RESTClient().Get()
				denied(req.Namespace(ns).Resource("services").Name("any:80").SubResource("proxy").Do(ctx).Error())
			}
		}
	})

	It("denies exec into a worker pod, for every identity", func(ctx SpecContext) {
		// Admission runs before the API server opens the connection, so this needs no kubelet.
		for _, user := range []string{"admin", workerController, other} {
			req := as[user].CoreV1().RESTClient().Post().Param("command", "id").Param("stdout", "true")
			denied(req.Namespace(workerNamespace).Resource("pods").Name(existing).SubResource("exec").Do(ctx).Error())
		}
	})
})

// renderWorkerPolicy renders the chart's worker policies with the Substrate driver, or skips the
// calling container while the chart has no such template. A skip becomes a failure under
// KOINON_REQUIRE=substrate-policy.
func renderWorkerPolicy(ctx context.Context) ([]*admissionv1.ValidatingAdmissionPolicy, []*admissionv1.ValidatingAdmissionPolicyBinding) {
	GinkgoHelper()
	helm, err := exec.LookPath(envOr("HELM", "helm"))
	Expect(err).NotTo(HaveOccurred(), "helm renders the chart's policies")
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, helm, "template", "koinon", "../../charts/koinon", "--namespace", "koinon",
		"--set", "execution.driver=substrate",
		"--set", "execution.substrate.controlAddress=api.ate-system.svc:443",
		"--set", "execution.substrate.routerAddress=atenet-router.ate-system.svc:80",
		"--set", "execution.substrate.tokenAudience=ateapi",
		"--set", "execution.substrate.allowUnhardened=true",
		"--set", "execution.substrate.workerNetworkPolicy.enabled=false",
		"--set", "execution.substrate.defaultTier=dev",
		// The default exempts the platform controllers from the digest rule (worker images are tags in
		// Substrate v1.0.0, #1140). The specs test the rule itself, so they exempt nobody.
		"--set", "execution.substrate.workerPolicy.digestExemptIdentities={}",
		"--set", "sandboxObserver.enabled=false", "--set", "sandboxNetworkPolicy.enabled=false", "--set", "sandboxLimitRange.enabled=false",
		"--show-only", workerPolicyTemplate)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "could not find template") {
			if testenv.Required(os.Getenv(testenv.RequireVar), "substrate-policy") {
				Fail("the chart renders no worker policy (" + workerPolicyTemplate + ", issue #1089), and KOINON_REQUIRE has substrate-policy")
			}
			Skip(testenv.Message("substrate-policy", err) + ": the chart renders no worker policy yet (issue #1089)")
		}
		Fail("helm template: " + err.Error() + ": " + stderr.String())
	}
	var (
		policies []*admissionv1.ValidatingAdmissionPolicy
		bindings []*admissionv1.ValidatingAdmissionPolicyBinding
	)
	for _, doc := range strings.Split(out.String(), "\n---") {
		var meta metav1.TypeMeta
		Expect(yaml.Unmarshal([]byte(doc), &meta)).To(Succeed())
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			p := &admissionv1.ValidatingAdmissionPolicy{}
			Expect(yaml.UnmarshalStrict([]byte(doc), p)).To(Succeed())
			policies = append(policies, p)
		case "ValidatingAdmissionPolicyBinding":
			b := &admissionv1.ValidatingAdmissionPolicyBinding{}
			Expect(yaml.UnmarshalStrict([]byte(doc), b)).To(Succeed())
			bindings = append(bindings, b)
		case "":
		default:
			Fail("unexpected " + meta.Kind + " in " + workerPolicyTemplate)
		}
	}
	Expect(policies).NotTo(BeEmpty(), "worker policies")
	Expect(bindings).NotTo(BeEmpty(), "worker policy bindings")
	return policies, bindings
}
