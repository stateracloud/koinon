// SPDX-License-Identifier: Apache-2.0

// Package preflight checks a cluster for Koinon's install prerequisites (plan item H2, ADRs
// 0048 and 0049): Agent Sandbox, the OpenShell gateway, the sandbox RuntimeClass, the CNPG
// operator in cnpg mode, the agentgateway GatewayClass in kubernetes mode and Kata guest
// seccomp, and whether the cluster enforces NetworkPolicy. Every check only reads, except the
// probes: the seccomp probe runs one short-lived unprivileged pod, the NetworkPolicy probe two
// pods and a NetworkPolicy in the release namespace. A missing prerequisite fails; guest
// seccomp, NetworkPolicy enforcement and the gateway's sandbox image settings only warn.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/project"
)

// Supported prerequisite versions.
const (
	// AgentSandboxAPIVersion is the agents.x-k8s.io version Koinon's objects use.
	AgentSandboxAPIVersion = "v1beta1"
	// CNPGAPIVersion is the postgresql.cnpg.io version of the chart's Cluster.
	CNPGAPIVersion = "v1"
	// AttachableLabel marks a RuntimeClass OpenShell may use for sandboxes.
	AttachableLabel = "openshell.ai/sandbox-attachable"

	sandboxCRD = "sandboxes.agents.x-k8s.io"
	cnpgCRD    = "clusters.postgresql.cnpg.io"

	agentSandboxInstall = "kubectl apply --server-side -f " +
		"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.10/sandbox-with-extensions.yaml"
)

// AgentgatewayAPIVersion is the agentgateway.dev version Koinon's objects use, and
// agentgatewayVersion matches the tested controller line, v1.7.x (plan item G2).
const AgentgatewayAPIVersion = "v1alpha1"

// agentgatewayControllerName is the controllerName of agentgateway's GatewayClass (verified on a
// kind cluster with controller v1.7.0; research/agentgateway-k8s-crds.md).
const agentgatewayControllerName = "agentgateway.dev/agentgateway"

var (
	agentgatewayVersion = regexp.MustCompile(`^v1\.7\.[0-9]+$`)
	agentgatewayCRDs    = []string{
		"agentgatewaybackends.agentgateway.dev", "agentgatewaymodels.agentgateway.dev",
		"agentgatewayparameters.agentgateway.dev", "agentgatewaypolicies.agentgateway.dev",
	}
)

const agentgatewayInstall = "helm upgrade -i agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds --version v1.7.0 -n agentgateway-system --create-namespace " +
	"&& helm upgrade -i agentgateway oci://cr.agentgateway.dev/charts/agentgateway --version v1.7.0 -n agentgateway-system"

// agentSandboxVersion matches the supported Agent Sandbox releases, v1.0.x.
var agentSandboxVersion = regexp.MustCompile(`^v1\.0\.[0-9]+$`)

// SupportedOpenShellVersions are the OpenShell gateway versions Koinon v0.1 is built against
// (the vendored protos in third_party/openshell).
var SupportedOpenShellVersions = []string{"0.1.7"}

var (
	crdGVR          = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	gatewayClassGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}
)

// Status is a check's outcome.
type Status int

// Check outcomes. Only Fail makes the preflight fail.
const (
	Pass Status = iota
	Warn
	Fail
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// Result is one check's outcome with a message for the operator and, unless it passed, how
// to fix it.
type Result struct {
	Check   string
	Status  Status
	Message string
	Fix     string
}

// Config is what the release expects of the cluster, from the chart's values.
type Config struct {
	// PostgresMode is postgres.mode.
	PostgresMode project.PostgresMode
	// GatewayMode is gateway.mode.
	GatewayMode project.GatewayMode
	// GatewayClassName is gateway.kubernetes.gatewayClassName.
	GatewayClassName string
	// RuntimeClassName is openshell.runtimeClassName.
	RuntimeClassName string
	// OpenShellAddress is openshell.address, for messages.
	OpenShellAddress string
	// OpenShellPlaintext is true when the gateway is dialled without TLS (openshell.insecure=true):
	// a healthy gateway is then reported as a warning.
	OpenShellPlaintext bool
	// AgentSandboxNamespace and AgentSandboxController locate the Agent Sandbox controller
	// Deployment, whose image tag carries the release version.
	AgentSandboxNamespace  string
	AgentSandboxController string
	// AgentgatewayNamespace and AgentgatewayController locate agentgateway's controller Deployment
	// (kubernetes mode), whose image tag carries its release.
	AgentgatewayNamespace  string
	AgentgatewayController string
	// OpenShellConfigNamespace and OpenShellConfigMap locate OpenShell's gateway configuration
	// (preflight.openshell). An empty ConfigMap skips the sandbox image checks.
	OpenShellConfigNamespace string
	OpenShellConfigMap       string
	// OpenShellWorkload is the name of the gateway's StatefulSet (or Deployment) in OpenShellConfigNamespace,
	// read for OPENSHELL_K8S_SA_TOKEN_TTL_SECS; empty means the variable is not checked.
	OpenShellWorkload string
	// ExpectSandboxPullSecrets is true when global.imagePullSecrets, global.imageRegistry or
	// global.registryMirrors is set: the gateway should then have sandbox pull secrets.
	ExpectSandboxPullSecrets bool
	// SupervisorImages is the effective sandboxPolicy.supervisorImages; empty when the policy is off.
	SupervisorImages []string
	// SupervisorTokenMaxSeconds is sandboxPolicy.supervisorTokenMaxSeconds; 0 when the policy is off. The
	// gateway's supervisor token lifetime above it fails the preflight (every supervisor pod is denied).
	SupervisorTokenMaxSeconds int
	// OpenShellGatewayID is openshell.gatewayId: compared with the gateway's gateway_id, because the
	// sandbox observer derives the workspace namespaces from it (ADR 0078). Empty skips the check.
	OpenShellGatewayID string
	// SandboxPolicyScopedToGateway is sandboxPolicy.enabled with scopeToGateway: the admission policies
	// bind only to namespaces labelled openshell.ai/gateway-id=<openshell.gatewayId>, so a mismatch with
	// the gateway's gateway_id is a Fail (the policies would match no sandbox namespace).
	SandboxPolicyScopedToGateway bool
	// SandboxObserverDisabled is sandboxObserver.enabled=false: the preflight warns that a sandbox that
	// cannot pull its image will show an unknown cause.
	SandboxObserverDisabled bool
}

// OpenShell is the part of the OpenShell gateway API the preflight calls.
type OpenShell interface {
	Health(ctx context.Context) (*openshellv1.HealthResponse, error)
}

// Prober runs the guest seccomp probe.
type Prober interface {
	Probe(ctx context.Context) (ProbeResult, error)
}

// Checker runs the checks.
type Checker struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// OpenShell is nil when no gateway address is configured.
	OpenShell OpenShell
	// Prober is nil when the seccomp probe is disabled.
	Prober Prober
	// NetPol is nil when the NetworkPolicy probe is disabled.
	NetPol NetPolProber
	Config Config
	// CallTimeout bounds each API and gateway call; the probes have their own timeouts.
	CallTimeout time.Duration
}

// Run runs every check that applies to the configuration and reports them in a fixed order.
// The NetworkPolicy probe runs alongside the other checks, so that the two probes' timeouts do
// not add up against the preflight Job's deadline.
func (c *Checker) Run(ctx context.Context) []Result {
	netpol := make(chan Result, 1)
	go func() { netpol <- c.checkNetworkPolicy(ctx) }()
	results := []Result{
		c.checkAgentSandbox(ctx),
		c.checkOpenShell(ctx),
		c.checkRuntimeClass(ctx),
	}
	results = append(results, c.checkSandboxImages(ctx)...)
	results = append(results, c.checkSandboxObserver()...)
	if c.Config.PostgresMode == project.PostgresCNPG {
		results = append(results, c.checkCNPG(ctx))
	}
	if c.Config.GatewayMode == project.GatewayKubernetes {
		results = append(results, c.checkGatewayClass(ctx))
	}
	return append(results, c.checkGuestSeccomp(ctx), <-netpol)
}

func (c *Checker) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := c.CallTimeout
	if d <= 0 {
		d = 15 * time.Second
	}
	return context.WithTimeout(ctx, d)
}

// apiFailure turns an API error other than NotFound into a failed result.
func apiFailure(check, what string, err error) Result {
	if apierrors.IsForbidden(err) {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("the preflight ServiceAccount may not read %s: %v", what, err),
			Fix:     "the chart grants this; check that the preflight ClusterRole was not changed",
		}
	}
	return Result{Check: check, Status: Fail, Message: fmt.Sprintf("reading %s: %v", what, err)}
}

func (c *Checker) getCRD(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	return c.Dynamic.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
}

// servedVersions lists the versions a CRD serves.
func servedVersions(crd *unstructured.Unstructured) []string {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	var served []string
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		if ok, _, _ := unstructured.NestedBool(m, "served"); ok && name != "" {
			served = append(served, name)
		}
	}
	return served
}

func (c *Checker) checkAgentSandbox(ctx context.Context) Result {
	const check = "Agent Sandbox"
	crd, err := c.getCRD(ctx, sandboxCRD)
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRD %s not found: Agent Sandbox is not installed", sandboxCRD),
			Fix:     "install Agent Sandbox v1.0.x: " + agentSandboxInstall,
		}
	case err != nil:
		return apiFailure(check, "CRD "+sandboxCRD, err)
	}
	served := servedVersions(crd)
	if !slices.Contains(served, AgentSandboxAPIVersion) {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRD %s serves %v, not %s: unsupported Agent Sandbox version", sandboxCRD, served, AgentSandboxAPIVersion),
			Fix:     "install Agent Sandbox v1.0.x: " + agentSandboxInstall,
		}
	}

	ns, name := c.Config.AgentSandboxNamespace, c.Config.AgentSandboxController
	dctx, cancel := c.callCtx(ctx)
	defer cancel()
	deploy, err := c.Kube.AppsV1().Deployments(ns).Get(dctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRDs present but controller Deployment %s/%s not found", ns, name),
			Fix:     "install Agent Sandbox v1.0.x, or set preflight.agentSandbox.namespace and .controller to where its controller runs",
		}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("Deployment %s/%s", ns, name), err)
	}
	var image string
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		if image == "" || strings.Contains(ctr.Image, "agent-sandbox-controller") {
			image = ctr.Image
		}
	}
	tag := imageTag(image)
	switch {
	case tag == "":
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("controller image %q has no tag, so its version is unknown (Koinon v0.1 supports v1.0.x)", image),
			Fix:     "deploy the controller from a tagged v1.0.x image",
		}
	case !agentSandboxVersion.MatchString(tag):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("controller version %s is not supported; Koinon v0.1 supports v1.0.x", tag),
			Fix:     "install Agent Sandbox v1.0.x: " + agentSandboxInstall,
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("%s serves %s; controller %s", sandboxCRD, AgentSandboxAPIVersion, tag)}
}

// imageTag returns the tag of an image reference, or "" when it has none.
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	last := ref[strings.LastIndex(ref, "/")+1:]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		return last[i+1:]
	}
	return ""
}

// normalizeVersion strips a leading "v" and build metadata: "v0.1.7+abc" is "0.1.7".
func normalizeVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	return v
}

func (c *Checker) checkOpenShell(ctx context.Context) Result {
	const check = "OpenShell gateway"
	if c.OpenShell == nil {
		return Result{
			Check: check, Status: Fail,
			Message: "no gateway address configured",
			Fix:     "set openshell.address to the OpenShell gateway's host:port",
		}
	}
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.OpenShell.Health(ctx)
	if err != nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("gateway at %s not reachable: %v", c.Config.OpenShellAddress, err),
			Fix: "install OpenShell " + strings.Join(SupportedOpenShellVersions, ", ") +
				", check openshell.address and that openshell.clientTLSSecret holds a client certificate the gateway accepts",
		}
	}
	version := normalizeVersion(resp.GetVersion())
	if !slices.Contains(SupportedOpenShellVersions, version) {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("gateway version %q is not supported; Koinon v0.1 supports %s",
				resp.GetVersion(), strings.Join(SupportedOpenShellVersions, ", ")),
			Fix: "install OpenShell " + strings.Join(SupportedOpenShellVersions, ", "),
		}
	}
	switch resp.GetStatus() {
	case openshellv1.ServiceStatus_SERVICE_STATUS_HEALTHY:
		if c.Config.OpenShellPlaintext {
			return Result{
				Check: check, Status: Warn,
				Message: fmt.Sprintf("%s healthy, version %s, but connected without TLS", c.Config.OpenShellAddress, version),
				Fix: "set openshell.clientTLSSecret so that sandbox create, exec and policy calls use mTLS; " +
					"openshell.insecure=true is only for a service mesh that provides mTLS",
			}
		}
		return Result{Check: check, Status: Pass, Message: fmt.Sprintf("%s healthy, version %s", c.Config.OpenShellAddress, version)}
	case openshellv1.ServiceStatus_SERVICE_STATUS_DEGRADED:
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("%s reports degraded, version %s", c.Config.OpenShellAddress, version),
			Fix:     "check the OpenShell gateway's logs",
		}
	default:
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("%s reports status %s", c.Config.OpenShellAddress, resp.GetStatus()),
			Fix:     "check the OpenShell gateway's logs",
		}
	}
}

func (c *Checker) checkRuntimeClass(ctx context.Context) Result {
	name := c.Config.RuntimeClassName
	check := fmt.Sprintf("RuntimeClass %q", name)
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	rc, err := c.Kube.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: "not found: the sandbox runtime (Kata, ADR 0045) is not installed or has another name",
			Fix:     "install Kata Containers, or set openshell.runtimeClassName to its RuntimeClass",
		}
	case err != nil:
		return apiFailure(check, "RuntimeClass "+name, err)
	}
	if rc.Labels[AttachableLabel] != "true" {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("label %s=\"true\" missing: OpenShell refuses sandboxes on it", AttachableLabel),
			Fix:     fmt.Sprintf("kubectl label runtimeclass %s %s=true", name, AttachableLabel),
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("handler %s, %s=true", rc.Handler, AttachableLabel)}
}

func (c *Checker) checkCNPG(ctx context.Context) Result {
	const check = "CloudNativePG"
	crd, err := c.getCRD(ctx, cnpgCRD)
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRD %s not found: the CNPG operator is not installed (postgres.mode=cnpg)", cnpgCRD),
			Fix:     "install the CloudNativePG operator, or set postgres.mode=external",
		}
	case err != nil:
		return apiFailure(check, "CRD "+cnpgCRD, err)
	}
	if served := servedVersions(crd); !slices.Contains(served, CNPGAPIVersion) {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRD %s serves %v, not %s", cnpgCRD, served, CNPGAPIVersion),
			Fix:     "upgrade the CloudNativePG operator",
		}
	}
	return c.checkCNPGOperator(ctx, check)
}

// checkCNPGOperator checks that the CNPG operator is running, not only that its CRDs exist.
// The chart's Cluster goes through the operator's admission webhooks, whose failure policy is
// Fail, so an operator that is not running fails the install with an opaque webhook error.
// The webhook configuration covering postgresql.cnpg.io clusters names the operator's
// Service wherever and however the operator was installed (manifests, Helm chart, OLM), and
// the Service having a ready endpoint means an operator pod serves it. This needs no
// cluster-wide read of Deployments (which would expose every pod spec), and does not depend
// on the operator's Deployment name or labels, which differ between install methods.
func (c *Checker) checkCNPGOperator(ctx context.Context, check string) Result {
	const fix = "install the CloudNativePG operator and wait for it to be ready, or set postgres.mode=external"
	lctx, cancel := c.callCtx(ctx)
	defer cancel()
	hooks, err := c.Kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(lctx, metav1.ListOptions{})
	if err != nil {
		return apiFailure(check, "ValidatingWebhookConfigurations", err)
	}
	var svc *admissionregistrationv1.ServiceReference
	for _, cfg := range hooks.Items {
		for _, h := range cfg.Webhooks {
			if h.ClientConfig.Service != nil && coversCNPGClusters(h.Rules) {
				svc = h.ClientConfig.Service
				break
			}
		}
		if svc != nil {
			break
		}
	}
	if svc == nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRD %s present but no admission webhook validates it: the CNPG operator is not installed", cnpgCRD),
			Fix:     fix,
		}
	}
	ectx, cancel := c.callCtx(ctx)
	defer cancel()
	eps, err := c.Kube.DiscoveryV1().EndpointSlices(svc.Namespace).List(ectx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + svc.Name,
	})
	if err != nil {
		return apiFailure(check, fmt.Sprintf("EndpointSlices of Service %s/%s", svc.Namespace, svc.Name), err)
	}
	for _, sl := range eps.Items {
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return Result{Check: check, Status: Pass, Message: fmt.Sprintf("%s serves %s; operator webhook %s/%s ready",
					cnpgCRD, CNPGAPIVersion, svc.Namespace, svc.Name)}
			}
		}
	}
	return Result{
		Check: check, Status: Fail,
		Message: fmt.Sprintf("the CNPG operator is not running: its webhook Service %s/%s has no ready endpoints", svc.Namespace, svc.Name),
		Fix:     fmt.Sprintf("check the operator's pods with kubectl -n %s get deployments,pods; %s", svc.Namespace, fix),
	}
}

// coversCNPGClusters reports whether webhook rules match postgresql.cnpg.io clusters.
func coversCNPGClusters(rules []admissionregistrationv1.RuleWithOperations) bool {
	for _, r := range rules {
		if (slices.Contains(r.APIGroups, "postgresql.cnpg.io") || slices.Contains(r.APIGroups, "*")) &&
			(slices.Contains(r.Resources, "clusters") || slices.Contains(r.Resources, "*")) {
			return true
		}
	}
	return false
}

func (c *Checker) checkGatewayClass(ctx context.Context) Result {
	name := c.Config.GatewayClassName
	check := fmt.Sprintf("GatewayClass %q", name)
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	gc, err := c.Dynamic.Resource(gatewayClassGVR).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: "not found: agentgateway's Kubernetes controller is not installed (gateway.mode=kubernetes)",
			Fix:     "install agentgateway in Kubernetes mode, set gateway.kubernetes.gatewayClassName, or use gateway.mode=standalone",
		}
	case err != nil:
		return apiFailure(check, "GatewayClass "+name, err)
	}
	// A class of that name that another controller serves is not agentgateway's.
	if controller, _, _ := unstructured.NestedString(gc.Object, "spec", "controllerName"); controller != agentgatewayControllerName {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("served by controller %q, not %s: this GatewayClass is not agentgateway's", controller, agentgatewayControllerName),
			Fix:     "set gateway.kubernetes.gatewayClassName to the GatewayClass of agentgateway's controller, or use gateway.mode=standalone",
		}
	}
	conditions, _, _ := unstructured.NestedSlice(gc.Object, "status", "conditions")
	for _, cond := range conditions {
		m, ok := cond.(map[string]any)
		if !ok || m["type"] != "Accepted" {
			continue
		}
		if m["status"] == "True" {
			controller, _, _ := unstructured.NestedString(gc.Object, "spec", "controllerName")
			return c.checkAgentgateway(ctx, check, controller)
		}
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("not accepted: %v: %v", m["reason"], m["message"]),
			Fix:     "check the agentgateway controller's logs",
		}
	}
	return Result{
		Check: check, Status: Fail,
		Message: "no Accepted condition: no controller has picked it up",
		Fix:     "check that the agentgateway controller is running",
	}
}

// checkAgentgateway checks what Koinon's native objects need beyond an accepted GatewayClass: the four
// agentgateway.dev CRDs at v1alpha1 and a controller of the tested v1.7 line. There is no fallback
// to standalone mode: a missing piece fails the install.
func (c *Checker) checkAgentgateway(ctx context.Context, check, controllerName string) Result {
	for _, name := range agentgatewayCRDs {
		crd, err := c.getCRD(ctx, name)
		switch {
		case apierrors.IsNotFound(err):
			return Result{
				Check: check, Status: Fail,
				Message: fmt.Sprintf("CRD %s not found: the agentgateway CRDs are not installed", name),
				Fix:     "install the agentgateway CRDs and controller v1.7.x: " + agentgatewayInstall,
			}
		case err != nil:
			return apiFailure(check, "CRD "+name, err)
		}
		if served := servedVersions(crd); !slices.Contains(served, AgentgatewayAPIVersion) {
			return Result{
				Check: check, Status: Fail,
				Message: fmt.Sprintf("CRD %s serves %v, not %s: unsupported agentgateway version", name, served, AgentgatewayAPIVersion),
				Fix:     "install agentgateway v1.7.x: " + agentgatewayInstall,
			}
		}
	}
	ns, name := c.Config.AgentgatewayNamespace, c.Config.AgentgatewayController
	dctx, cancel := c.callCtx(ctx)
	defer cancel()
	deploy, err := c.Kube.AppsV1().Deployments(ns).Get(dctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("controller Deployment %s/%s not found, so its version is unknown", ns, name),
			Fix:     "install agentgateway v1.7.x, or set preflight.agentgateway.namespace and .controller to where its controller runs",
		}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("Deployment %s/%s", ns, name), err)
	}
	var image string
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		if image == "" || strings.Contains(ctr.Image, "controller") {
			image = ctr.Image
		}
	}
	tag := imageTag(image)
	if !agentgatewayVersion.MatchString(tag) {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("controller image %q is not a tested release (kubernetes mode is tested with v1.7.x)", image),
			Fix:     "install agentgateway v1.7.x: " + agentgatewayInstall,
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("accepted by %s; CRDs agentgateway.dev/%s; controller %s", controllerName, AgentgatewayAPIVersion, tag)}
}

// verifySeccomp tells the operator how to check guest seccomp by hand.
func verifySeccomp(runtimeClass string) string {
	return fmt.Sprintf("verify by hand: run a pod with runtimeClassName %s and seccompProfile RuntimeDefault that "+
		"runs `grep Seccomp: /proc/self/status`; 2 means on, 0 means off. To enable it, set "+
		"disable_guest_seccomp = false in the Kata configuration on every node (ADR 0048)", runtimeClass)
}

func (c *Checker) checkGuestSeccomp(ctx context.Context) Result {
	const check = "Kata guest seccomp"
	if c.Prober == nil {
		return Result{
			Check: check, Status: Warn,
			Message: "not checked (preflight.seccompProbe.enabled=false)",
			Fix:     verifySeccomp(c.Config.RuntimeClassName),
		}
	}
	res, err := c.Prober.Probe(ctx)
	if err != nil {
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("probe pod did not report: %v", err),
			Fix:     verifySeccomp(c.Config.RuntimeClassName),
		}
	}
	if res.Seccomp != 2 || res.Keyctl != "EPERM" {
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("off: a %s pod with RuntimeDefault runs with Seccomp: %d and keyctl %s "+
				"(want 2 and EPERM); sandboxes rely on OpenShell's filter alone, which allows keyctl and add_key",
				c.Config.RuntimeClassName, res.Seccomp, res.Keyctl),
			Fix: "set disable_guest_seccomp = false in the Kata configuration on every node (ADR 0048)",
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("on: Seccomp: 2, keyctl EPERM in a %s pod", c.Config.RuntimeClassName)}
}

// NetPolNotEnforced is the warning when a denied connection got through.
const NetPolNotEnforced = "NetworkPolicy is not enforced in this cluster; the OTLP receiver and gateway access rules are advisory"

// verifyNetPol tells the operator how to check NetworkPolicy enforcement by hand.
const verifyNetPol = "verify by hand: apply a NetworkPolicy that denies ingress to a test pod and check that " +
	"another pod can no longer connect to it. Enforcement needs a network plugin that implements " +
	"NetworkPolicy (any one; Koinon depends on none in particular)"

func (c *Checker) checkNetworkPolicy(ctx context.Context) Result {
	const check = "NetworkPolicy enforcement"
	if c.NetPol == nil {
		return Result{
			Check: check, Status: Warn,
			Message: "not checked (preflight.networkPolicyProbe.enabled=false)",
			Fix:     verifyNetPol,
		}
	}
	res, err := c.NetPol.Probe(ctx)
	switch {
	case err != nil:
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("probe did not report: %v", err),
			Fix:     verifyNetPol,
		}
	case !res.ControlReached:
		return Result{
			Check: check, Status: Warn,
			Message: "inconclusive: the probe client could not reach the probe server on the port its NetworkPolicy allows",
			Fix:     "check for other NetworkPolicies or a firewall between pods in the release namespace; " + verifyNetPol,
		}
	case res.DeniedReached:
		return Result{
			Check: check, Status: Warn,
			Message: NetPolNotEnforced,
			Fix:     "install a network plugin that enforces NetworkPolicy, or restrict access to the OTLP receiver and agentgateway another way",
		}
	}
	return Result{Check: check, Status: Pass, Message: "a NetworkPolicy denying ingress blocked a probe connection in the release namespace"}
}

// Write prints the results for the operator and returns how many failed.
func Write(w io.Writer, results []Result) (failed int) {
	var warned int
	_, _ = fmt.Fprintln(w, "Koinon install preflight")
	for _, r := range results {
		_, _ = fmt.Fprintf(w, "  %s  %s: %s\n", r.Status, r.Check, r.Message)
		if r.Status != Pass && r.Fix != "" {
			_, _ = fmt.Fprintf(w, "        fix: %s\n", r.Fix)
		}
		switch r.Status {
		case Fail:
			failed++
		case Warn:
			warned++
		}
	}
	switch {
	case failed > 0:
		_, _ = fmt.Fprintf(w, "Preflight failed: %d of %d checks failed, %d warnings. Fix the above, or set "+
			"preflight.enabled=false to skip the checks.\n", failed, len(results), warned)
	case warned > 0:
		_, _ = fmt.Fprintf(w, "Preflight passed with %d warnings.\n", warned)
	default:
		_, _ = fmt.Fprintln(w, "Preflight passed.")
	}
	return failed
}

// ErrFailed is returned when at least one check failed.
var ErrFailed = errors.New("preflight failed")
