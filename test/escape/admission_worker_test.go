// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/preflight"
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
const (
	otherDigest  = "registry.invalid/koinon-escape/other@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	workerDigest = "registry.invalid/koinon-escape/worker@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	// A worker image of the micro-VM class (workerShapes.microvm.images); workerDigest is of the gVisor class.
	microvmDigest = "registry.invalid/koinon-escape/worker-microvm@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

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
		asDyn  map[string]dynamic.Interface
		dryRun = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	)
	const (
		other = "system:serviceaccount:koinon:koinon-reconciler"
		// What kubeadm and kind give their admin: not system:masters.
		kubeadmAdmin = "kubernetes-admin"
		existing     = "existing-worker"
		// The chart's NetworkPolicy of the worker namespace (release koinon), and a per-pool policy of the controller.
		chartNetpol = "koinon-substrate-workers"
		poolNetpol  = "pool-egress"
		gcUser      = "system:serviceaccount:kube-system:generic-garbage-collector"
		// The hook Job of issue #1191 (charts/koinon/templates/substrate-imagecache-ca.yaml). Not an installer identity.
		caHook = "system:serviceaccount:koinon:koinon-imagecache-ca"
	)

	isTrue := true
	// The shape that Substrate sets on the worker container (workerpool_apply.go:377-468): the gVisor class by default.
	gvisorCaps := []corev1.Capability{"NET_ADMIN", "SYS_ADMIN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "CHOWN", "NET_RAW", "SETFCAP"}
	microvmCaps := []corev1.Capability{"NET_ADMIN", "SYS_ADMIN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "CHOWN", "MKNOD", "SETFCAP", "FSETID", "SETPCAP"}
	shape := func(add []corev1.Capability) *corev1.SecurityContext {
		root, no := int64(0), false
		return &corev1.SecurityContext{
			RunAsUser: &root, RunAsGroup: &root, Privileged: &no, AllowPrivilegeEscalation: &no,
			Capabilities:    &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: add},
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		}
	}
	worker := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "11111111-1111-1111-1111-111111111111", Controller: &isTrue,
			}}},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: workerDigest, SecurityContext: shape(gvisorCaps)}},
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

		// A cluster-scoped Substrate kind: the objects policy sees it with no request.namespace key.
		env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{sandboxConfigCRD()}}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		asDyn = map[string]dynamic.Interface{"admin": dynamic.NewForConfigOrDie(cfg)}
		for _, user := range []string{workerController, replicaSetController, daemonSetController, other, kubeadmAdmin, gcUser, caHook} {
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
			asDyn[user] = dynamic.NewForConfigOrDie(c)
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

		// The SandboxConfig the objects cases act on exists before the policies do.
		_, err = asDyn["admin"].Resource(sandboxConfigGVR).Create(ctx, sandboxConfig("gvisor-default"), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// The pod the update and connect cases act on exists before the policies do.
		_, err = admin.CoreV1().Pods(workerNamespace).Create(ctx, worker(existing), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.CoreV1().Pods(controlNamespace).Create(ctx, worker(existing), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// The atelet DaemonSet and another one, which the CA stamp cases (issue 1191) update.
		for _, name := range []string{"atelet", "other-ds"} {
			_, err = admin.AppsV1().DaemonSets(controlNamespace).Create(ctx, &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}, Annotations: map[string]string{"keep": "me"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: workerDigest}}},
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		// The NetworkPolicies the guard cases act on exist before the policies do.
		for _, n := range []string{chartNetpol, poolNetpol} {
			_, err = admin.NetworkingV1().NetworkPolicies(workerNamespace).Create(ctx, netpolEgress(n, toNamespace(controlNamespace)), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

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
		// The NetworkPolicy guard (issue 1158): an identity that may not write is denied once the guard is active.
		Eventually(func() error {
			_, err := as[other].NetworkingV1().NetworkPolicies(workerNamespace).Create(ctx, netpolEgress("probe-netpol", toNamespace(controlNamespace)), dryRun)
			return err
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

	It("allows added capabilities and Unconfined profiles only in the exact shape of the worker container", func(ctx SpecContext) {
		sc := func(f func(*corev1.SecurityContext)) func(*corev1.Pod) {
			return func(p *corev1.Pod) { f(p.Spec.Containers[0].SecurityContext) }
		}
		// Both classes of Substrate v1.0.1.
		Expect(create(ctx, replicaSetController, workerNamespace, worker("shape-gvisor"))).To(Succeed())
		Expect(create(ctx, replicaSetController, workerNamespace, worker("shape-microvm", func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = microvmDigest
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = microvmCaps
		}))).To(Succeed())
		// The capability set is the set of the class of the image (#1176): not the set of the other class.
		denied(create(ctx, replicaSetController, workerNamespace, worker("gvisor-image-microvm-caps", sc(func(c *corev1.SecurityContext) {
			c.Capabilities.Add = microvmCaps
		}))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("microvm-image-gvisor-caps", func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = microvmDigest
		})))
		// One capability more or less than the class, and the old ceiling alone.
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-more", sc(func(c *corev1.SecurityContext) {
			c.Capabilities.Add = append(append([]corev1.Capability{}, gvisorCaps...), "SYS_PTRACE")
		}))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-less", sc(func(c *corev1.SecurityContext) {
			c.Capabilities.Add = gvisorCaps[:len(gvisorCaps)-1]
		}))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-ceiling-only", sc(func(c *corev1.SecurityContext) {
			c.Capabilities.Add = []corev1.Capability{"SYS_ADMIN", "NET_ADMIN"}
		}))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-all", sc(func(c *corev1.SecurityContext) {
			c.Capabilities.Add = []corev1.Capability{"ALL"}
		}))))
		// The rest of the shape.
		denied(create(ctx, replicaSetController, workerNamespace, worker("no-drop", sc(func(c *corev1.SecurityContext) { c.Capabilities.Drop = nil }))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("escalation", sc(func(c *corev1.SecurityContext) {
			yes := true
			c.AllowPrivilegeEscalation = &yes
		}))))
		denied(create(ctx, replicaSetController, workerNamespace, worker("not-root", sc(func(c *corev1.SecurityContext) {
			uid := int64(1000)
			c.RunAsUser = &uid
		}))))
		// A worker container with no security context has no shape.
		denied(create(ctx, replicaSetController, workerNamespace, worker("bare", func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext = nil })))
		// Another container gets no capability and no Unconfined profile, whatever its image.
		denied(create(ctx, replicaSetController, workerNamespace, worker("sidecar-caps", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: workerDigest, SecurityContext: shape(gvisorCaps)})
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("sidecar-unconfined", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: workerDigest, SecurityContext: &corev1.SecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			}})
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("other-image-caps", func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = otherDigest
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("renamed-worker", func(p *corev1.Pod) {
			p.Spec.Containers[0].Name = "not-the-worker"
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("caps-init", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: workerDigest, SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_MODULE"}},
			}}}
		})))
		// An init container named worker is never the worker container (#1176). Kubernetes refuses a main container with
		// the same name, so the pod has no worker container, and the shape of the init container is refused.
		denied(create(ctx, replicaSetController, workerNamespace, worker("init-named-worker", func(p *corev1.Pod) {
			p.Spec.Containers[0] = corev1.Container{Name: "main", Image: otherDigest}
			p.Spec.InitContainers = []corev1.Container{{Name: "worker", Image: workerDigest, SecurityContext: shape(gvisorCaps)}}
		})))
		// Another container with no capability is fine.
		Expect(create(ctx, replicaSetController, workerNamespace, worker("sidecar-plain", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: otherDigest})
		}))).To(Succeed())
	})

	It("denies a privileged container and an unmasked /proc", func(ctx SpecContext) {
		yes := true
		unmasked := corev1.UnmaskedProcMount
		denied(create(ctx, replicaSetController, workerNamespace, worker("privileged", func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("proc-unmasked", func(p *corev1.Pod) {
			noHostUsers := false
			p.Spec.HostUsers = &noHostUsers
			p.Spec.Containers[0].SecurityContext.ProcMount = &unmasked
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("privileged-init", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: workerDigest, SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}
		})))
	})

	It("denies a Secret in a projected volume and in the environment", func(ctx SpecContext) {
		denied(create(ctx, replicaSetController, workerNamespace, worker("projected-secret", func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "s", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}}}},
			}}})
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("env-secret", func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}, Key: "k"}}}}
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("envfrom-secret", func(p *corev1.Pod) {
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}}}}
		})))
		Expect(create(ctx, replicaSetController, workerNamespace, worker("env-plain", func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "X", Value: "y"}}
		}))).To(Succeed())
	})

	It("denies hostPort, and an Unconfined profile on the pod or on a container that is not the worker", func(ctx SpecContext) {
		denied(create(ctx, replicaSetController, workerNamespace, worker("hostport", func(p *corev1.Pod) {
			p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("pod-apparmor-unconfined", func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}
		})))
		denied(create(ctx, replicaSetController, workerNamespace, worker("pod-seccomp-unconfined", func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
		})))
		// The worker container itself carries both profiles in the shape of Substrate (the control case above).
		Expect(create(ctx, replicaSetController, workerNamespace, worker("worker-unconfined"))).To(Succeed())
		Expect(create(ctx, replicaSetController, workerNamespace, worker("seccomp-runtime", func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
		}))).To(Succeed())
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
		It("admits each hostPath of the atelet manifest (Substrate v1.0.1 atelet.yaml), and no worker device for the node agent", func(ctx SpecContext) {
			for i, path := range []string{"/var/lib/ate", "/var/lib/kubelet/plugins", "/var/lib/kubelet/device-plugins", "/dev", "/home/kubernetes/bin", "/etc/srv/kubernetes/cri_auth_config.yaml"} {
				Expect(create(ctx, daemonSetController, controlNamespace, atelet(fmt.Sprintf("atelet-path-%d", i), func(p *corev1.Pod) { p.Spec.Volumes[0].HostPath.Path = path }))).To(Succeed())
			}
			// The worker list is not the node agent's list, and the reverse also holds.
			denied(create(ctx, replicaSetController, workerNamespace, worker("worker-dev", func(p *corev1.Pod) { p.Spec.Volumes[0].HostPath.Path = "/dev" })))
			denied(create(ctx, replicaSetController, workerNamespace, worker("worker-kubelet", func(p *corev1.Pod) { p.Spec.Volumes[0].HostPath.Path = "/var/lib/kubelet/plugins" })))
		})
		It("denies the same pod from another identity, with another owner or ServiceAccount, or with a hostPath outside its list", func(ctx SpecContext) {
			denied(create(ctx, workerController, controlNamespace, atelet("by-ate-controller")))
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
		// The installer patch of issue #1191: atelet gets the optional ConfigMap koinon-imagecache-ca, a read-only mount and
		// SSL_CERT_DIR, and nothing else. The policy is not changed for it; these specs show that it admits that shape and
		// keeps denying the shapes a weaker policy would let through (a CA by hostPath, a second image without a digest).
		It("admits atelet with the image cache CA as a ConfigMap volume, a read-only mount and SSL_CERT_DIR (issue 1191)", func(ctx SpecContext) {
			Expect(create(ctx, daemonSetController, controlNamespace, atelet("atelet-cache-ca", func(p *corev1.Pod) {
				optional := true
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "koinon-imagecache-ca", VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "koinon-imagecache-ca"}, Optional: &optional},
				}})
				c := &p.Spec.Containers[0]
				c.Env = append(c.Env, corev1.EnvVar{Name: "SSL_CERT_DIR", Value: "/etc/koinon/imagecache-ca:/etc/ssl/certs"})
				c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "koinon-imagecache-ca", MountPath: "/etc/koinon/imagecache-ca", ReadOnly: true})
			}))).To(Succeed())
		})
		It("still denies a CA by hostPath, and an unpinned image beside atelet (issue 1191)", func(ctx SpecContext) {
			denied(create(ctx, daemonSetController, controlNamespace, atelet("ca-unpinned-init-image", func(p *corev1.Pod) {
				p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: "ca", Image: "registry.invalid/koinon-escape/ca:v1"})
			})))
			denied(create(ctx, daemonSetController, controlNamespace, atelet("ca-host-path", func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/etc/ssl/certs"},
				}})
			})))
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

	// Issue #1176: Koinon's preflight probe pods run in a namespace of their own, not in Substrate's namespaces. The
	// policy has no exception for them. The real probe pods, with a pinned image, are still refused in both Substrate
	// namespaces, from the installer's identity: a bare pod there has no controller owner.
	It("still denies Koinon's probe pods in the control-plane and the worker namespace (issue 1176)", func(ctx SpecContext) {
		for _, ns := range []string{controlNamespace, workerNamespace} {
			prober := preflight.KubeSubstrateProber{
				Namespace: ns, NamePrefix: "koinon-preflight", Image: otherDigest,
				Labels: map[string]string{"app.kubernetes.io/part-of": "koinon"},
			}
			for _, pod := range []*corev1.Pod{prober.IssuerPod("https://kubernetes.default.svc"), prober.DevicesPod(), prober.UserNamespacesPod("node-a")} {
				denied(create(ctx, "admin", ns, pod))
			}
		}
	})

	// The CA hook Job of issue #1191 rolls atelet by one annotation on its pod template. The rule is narrow: one
	// ServiceAccount, one DaemonSet, one UPDATE, one annotation key. The ServiceAccount is not an installer identity.
	Describe("the image cache CA stamp (issue 1191)", func() {
		const stamp = "koinon.dev/imagecache-ca-sha256"
		update := func(ctx context.Context, user, name string, mutate func(*appsv1.DaemonSet)) error {
			GinkgoHelper()
			ds, err := admin.AppsV1().DaemonSets(controlNamespace).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			mutate(ds)
			_, err = as[user].AppsV1().DaemonSets(controlNamespace).Update(ctx, ds, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			return err
		}
		annotate := func(ds *appsv1.DaemonSet) {
			ds.Spec.Template.Annotations[stamp] = strings.Repeat("ab", 32)
		}
		It("lets the hook Job change only the CA stamp of the atelet pod template", func(ctx SpecContext) {
			Expect(update(ctx, caHook, "atelet", annotate)).To(Succeed())
		})
		It("denies every other change by the hook Job", func(ctx SpecContext) {
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Spec.Template.Spec.Containers[0].Image = "registry.invalid/x:v1" }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) {
				ds.Spec.Template.Spec.Containers[0].Env = append(ds.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "X", Value: "1"})
			}))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) {
				ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
			}))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) {
				privileged := true
				ds.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
			}))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Spec.Template.Annotations["other"] = "x" }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { delete(ds.Spec.Template.Annotations, "keep") }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Spec.Template.Labels["extra"] = "x" }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Labels = map[string]string{"x": "y"} }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Annotations = map[string]string{"x": "y"} }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) {
				ds.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "gone", UID: "22222222-2222-2222-2222-222222222222"}}
			}))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Finalizers = []string{"example.com/hold"} }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Spec.Template.Finalizers = []string{"example.com/hold"} }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Spec.MinReadySeconds = 3600 }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { limit := int32(1); ds.Spec.RevisionHistoryLimit = &limit }))
			denied(update(ctx, caHook, "atelet", func(ds *appsv1.DaemonSet) { ds.Spec.Template.Annotations[stamp] = "not-a-hash" }))
		})
		It("denies the stamp on another DaemonSet, and a create or a delete by the hook Job", func(ctx SpecContext) {
			denied(update(ctx, caHook, "other-ds", annotate))
			_, err := as[caHook].AppsV1().DaemonSets(controlNamespace).Create(ctx, &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "new-ds"},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "n"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "n"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "n", Image: workerDigest}}},
					},
				},
			}, dryRun)
			denied(err)
			denied(as[caHook].AppsV1().DaemonSets(controlNamespace).Delete(ctx, "atelet", metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}))
		})
		It("denies the stamp to every other identity", func(ctx SpecContext) {
			denied(update(ctx, other, "atelet", annotate))
			denied(update(ctx, workerController, "atelet", annotate))
		})
		It("denies the hook Job a stamp on a DaemonSet of the worker namespace", func(ctx SpecContext) {
			_, err := admin.AppsV1().DaemonSets(workerNamespace).Create(ctx, &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "atelet"},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "atelet"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "atelet"}, Annotations: map[string]string{}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "atelet", Image: workerDigest}}},
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ds, err := admin.AppsV1().DaemonSets(workerNamespace).Get(ctx, "atelet", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ds.Spec.Template.Annotations = map[string]string{stamp: strings.Repeat("ab", 32)}
			_, err = as[caHook].AppsV1().DaemonSets(workerNamespace).Update(ctx, ds, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			denied(err)
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

	It("lets the controller finalize a cluster-scoped SandboxConfig, and nothing else write it", func(ctx SpecContext) {
		// The object has no namespace: CEL has no request.namespace key for it (a read fails with "no such key").
		sc, err := asDyn["admin"].Resource(sandboxConfigGVR).Get(ctx, "gvisor-default", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		dry := metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}
		// A finalizer change is a metadata change: it keeps the spec.
		meta := sc.DeepCopy()
		meta.SetFinalizers([]string{"ate.dev/finalizer"})
		_, err = asDyn[workerController].Resource(sandboxConfigGVR).Update(ctx, meta, dry)
		Expect(err).NotTo(HaveOccurred())
		// A spec change is not.
		spec := sc.DeepCopy()
		Expect(unstructured.SetNestedField(spec.Object, "changed", "spec", "class")).To(Succeed())
		_, err = asDyn[workerController].Resource(sandboxConfigGVR).Update(ctx, spec, dry)
		denied(err)
		// Another identity may not write it at all.
		_, err = asDyn[other].Resource(sandboxConfigGVR).Update(ctx, meta, dry)
		denied(err)
		// The installer may.
		_, err = asDyn[kubeadmAdmin].Resource(sandboxConfigGVR).Update(ctx, spec, dry)
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

	It("lets only the installer exec, attach and port-forward in the control-plane namespace, and nobody in the worker namespace", func(ctx SpecContext) {
		connect := func(user, ns, sub string) error {
			req := as[user].CoreV1().RESTClient().Post().Param("command", "id").Param("stdout", "true")
			return req.Namespace(ns).Resource("pods").Name(existing).SubResource(sub).Do(ctx).Error()
		}
		// Admission lets the installer through. The API server then has no kubelet to dial, so the error is another one.
		for _, sub := range []string{"exec", "attach", "portforward"} {
			err := connect("admin", controlNamespace, sub)
			if err != nil {
				Expect(err.Error()).NotTo(ContainSubstring("ValidatingAdmissionPolicy"), sub+" for the installer in the control-plane namespace")
			}
			err = connect(kubeadmAdmin, controlNamespace, sub)
			if err != nil {
				Expect(err.Error()).NotTo(ContainSubstring("ValidatingAdmissionPolicy"), sub+" for the kubeadm admin")
			}
			for _, user := range []string{workerController, other, daemonSetController} {
				denied(connect(user, controlNamespace, sub))
			}
			// The worker namespace stays closed to the installer too.
			for _, user := range []string{"admin", kubeadmAdmin} {
				denied(connect(user, workerNamespace, sub))
			}
		}
		// The proxies stay closed in the control-plane namespace for the installer.
		denied(connect("admin", controlNamespace, "proxy"))
	})

	It("guards NetworkPolicy writes in the worker namespace (issue 1158)", func(ctx SpecContext) {
		write := func(user string, np *networkingv1.NetworkPolicy) error {
			_, err := as[user].NetworkingV1().NetworkPolicies(workerNamespace).Create(ctx, np, dryRun)
			return err
		}
		update := func(user string, np *networkingv1.NetworkPolicy) error {
			_, err := as[user].NetworkingV1().NetworkPolicies(workerNamespace).Update(ctx, np, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			return err
		}
		remove := func(user, name string) error {
			return as[user].NetworkingV1().NetworkPolicies(workerNamespace).Delete(ctx, name, metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
		}
		current := func(name string) *networkingv1.NetworkPolicy {
			np, err := admin.NetworkingV1().NetworkPolicies(workerNamespace).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			return np
		}

		// The installer writes anything, the controller writes egress to the control plane and to DNS.
		for _, user := range []string{"admin", kubeadmAdmin} {
			Expect(write(user, netpolEgress("by-"+strings.ReplaceAll(user, ":", "-"), toCIDR("0.0.0.0/0")))).To(Succeed(), user)
		}
		Expect(write(workerController, netpolEgress("ctl-control", toNamespace(controlNamespace)))).To(Succeed())
		Expect(write(workerController, netpolPorts("ctl-dns", toDNS(), 53))).To(Succeed())
		both := netpolEgress("ctl-both", toNamespace(controlNamespace))
		both.Spec.Egress = append(both.Spec.Egress, netpolPorts("x", toDNS(), 53).Spec.Egress...)
		Expect(write(workerController, both)).To(Succeed())
		// A narrower DNS peer is fine: a pod selector only narrows the namespace peer.
		kubeDNS := toDNS()
		kubeDNS.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}
		Expect(write(workerController, netpolPorts("ctl-kube-dns", kubeDNS, 53))).To(Succeed())
		fromControl := netpolEgress("ctl-ingress", toNamespace(controlNamespace))
		fromControl.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{toNamespace(controlNamespace), toNamespace("koinon")}}}
		Expect(write(workerController, fromControl)).To(Succeed())
		noEgress := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "ctl-none"}}
		Expect(write(workerController, noEgress)).To(Succeed(), "no egress rule")
		Expect(update(workerController, current(poolNetpol))).To(Succeed())
		Expect(remove(workerController, poolNetpol)).To(Succeed())

		// Every other identity is denied, a cluster admin outside the installer groups included.
		for _, user := range []string{other, daemonSetController, replicaSetController} {
			denied(write(user, netpolEgress("by-other", toNamespace(controlNamespace))))
			denied(update(user, current(poolNetpol)))
			denied(remove(user, poolNetpol))
		}

		// The controller's policies reach the control-plane namespace and DNS, and nothing else.
		wide := map[string]*networkingv1.NetworkPolicy{
			"allow-all-rule":      {Spec: networkingv1.NetworkPolicySpec{Egress: []networkingv1.NetworkPolicyEgressRule{{}}}},
			"empty-namespace":     netpolEgress("x", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{}}),
			"cidr":                netpolEgress("x", toCIDR("0.0.0.0/0")),
			"same-namespace-pods": netpolEgress("x", networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}),
			"other-namespace":     netpolEgress("x", toNamespace("kube-public")),
			"worker-namespace":    netpolEgress("x", toNamespace(workerNamespace)),
			"all-namespaces":      netpolEgress("x", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpExists}}}}),
			"expression-control":  netpolEgress("x", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": controlNamespace}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: metav1.LabelSelectorOpExists}}}}),
			"extra-label":         netpolEgress("x", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": controlNamespace, "a": "b"}}}),
			"other-label":         netpolEgress("x", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "ate"}}}),
			"dns-any-port":        netpolEgress("x", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}),
			"kube-system-443":     netpolPorts("x", toNamespace("kube-system"), 443),
			"dns-and-other-port":  netpolPorts("x", toNamespace("kube-system"), 53, 8080),
			"dns-named-port":      withPort(netpolPorts("x", toNamespace("kube-system"), 53), func(q *networkingv1.NetworkPolicyPort) { n := intstr.FromString("dns"); q.Port = &n }),
			"dns-port-range":      withPort(netpolPorts("x", toNamespace("kube-system"), 53), func(q *networkingv1.NetworkPolicyPort) { e := int32(60); q.EndPort = &e }),
			"ingress-any":         withIngress(netpolEgress("x", toNamespace(controlNamespace))),
			"ingress-cidr":        withIngress(netpolEgress("x", toNamespace(controlNamespace)), toCIDR("0.0.0.0/0")),
			"ingress-all-ns":      withIngress(netpolEgress("x", toNamespace(controlNamespace)), networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{}}),
			"ingress-other-ns":    withIngress(netpolEgress("x", toNamespace(controlNamespace)), toNamespace("kube-public")),
			"good-then-wide":      netpolEgress("x", toNamespace(controlNamespace), toCIDR("0.0.0.0/0")),
		}
		for name, np := range wide {
			np.Name = "wide-" + name
			denied(write(workerController, np))
			Expect(write("admin", np)).To(Succeed(), name+": the installer is not limited")
		}
		// An update that widens a policy is denied too.
		widened := current(poolNetpol)
		widened.Spec.Egress = append(widened.Spec.Egress, networkingv1.NetworkPolicyEgressRule{})
		denied(update(workerController, widened))
		Expect(update("admin", widened)).To(Succeed())

		// The chart's own policy is the floor: the controller neither writes nor deletes it.
		denied(update(workerController, current(chartNetpol)))
		// Kubernetes' controllers never create a policy.
		denied(write(gcUser, netpolEgress("by-gc", toNamespace(controlNamespace))))
		denied(remove(workerController, chartNetpol))
		Expect(update("admin", current(chartNetpol))).To(Succeed())
		Expect(remove("admin", chartNetpol)).To(Succeed())

		// Kubernetes' garbage collector deletes a policy and changes its metadata, never its spec.
		Expect(remove(gcUser, poolNetpol)).To(Succeed())
		meta := current(poolNetpol)
		meta.Labels = map[string]string{"gc": "yes"}
		Expect(update(gcUser, meta)).To(Succeed())
		denied(update(gcUser, widened))
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
		"--set", "execution.substrate.routerAddress=atenet-router.ate-system.svc:443",
		"--set", "execution.substrate.tokenAudience=ateapi",
		"--set", "execution.substrate.allowUnhardened=true",
		"--set", "execution.substrate.workerNetworkPolicy.enabled=false",
		"--set", "execution.substrate.defaultTier=dev",
		// The default exempts the platform controllers from the digest rule (worker images are tags in
		// Substrate v1.0.1, #1140). The specs test the rule itself, so they exempt nobody.
		"--set", "execution.substrate.workerPolicy.digestExemptIdentities={}",
		// The default WorkerPool has a tag image, which the chart refuses without the exemption; the specs
		// test the policy, not the pool.
		"--set", "execution.substrate.workerPool.enabled=false",
		"--set", "execution.substrate.workerPolicy.workerShapes.gvisor.images={"+workerDigest+"}",
		"--set", "execution.substrate.workerPolicy.workerShapes.microvm.images={"+microvmDigest+"}",
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

var sandboxConfigGVR = schema.GroupVersionResource{Group: "ate.dev", Version: "v1", Resource: "sandboxconfigs"}

// sandboxConfigCRD is a minimal cluster-scoped SandboxConfig: the shape of the kind matters, not its schema.
func sandboxConfigCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxConfigGVR.Resource + "." + sandboxConfigGVR.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: sandboxConfigGVR.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "sandboxconfigs", Singular: "sandboxconfig", Kind: "SandboxConfig", ListKind: "SandboxConfigList",
			},
			Scope: apiextensionsv1.ClusterScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: sandboxConfigGVR.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: new(true),
				}},
			}},
		},
	}
}

func sandboxConfig(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1", "kind": "SandboxConfig",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"class": "gvisor"},
	}}
}

// netpolEgress is a NetworkPolicy of the worker namespace with one egress rule that has the given peers.
func netpolEgress(name string, peers ...networkingv1.NetworkPolicyPeer) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers}},
		},
	}
}

// netpolPorts is a policy with one egress rule to peer on the given TCP ports.
func netpolPorts(name string, peer networkingv1.NetworkPolicyPeer, ports ...int) *networkingv1.NetworkPolicy {
	np := netpolEgress(name, peer)
	for _, p := range ports {
		port := intstr.FromInt32(int32(p))
		proto := corev1.ProtocolTCP
		np.Spec.Egress[0].Ports = append(np.Spec.Egress[0].Ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
	}
	return np
}

func toNamespace(ns string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
	}}
}

func toCIDR(cidr string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}
}

// toDNS is the peer of DNS (kube-system); a rule that uses it must name port 53 (netpolPorts).
func toDNS() networkingv1.NetworkPolicyPeer { return toNamespace("kube-system") }

func withPort(np *networkingv1.NetworkPolicy, mutate func(*networkingv1.NetworkPolicyPort)) *networkingv1.NetworkPolicy {
	mutate(&np.Spec.Egress[0].Ports[0])
	return np
}

// withIngress adds one ingress rule with the given peers (none: a rule that admits every source).
func withIngress(np *networkingv1.NetworkPolicy, from ...networkingv1.NetworkPolicyPeer) *networkingv1.NetworkPolicy {
	np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: from}}
	return np
}
