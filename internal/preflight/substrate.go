// SPDX-License-Identifier: Apache-2.0

package preflight

// The Substrate driver's checks (ADR 0119 D4 and D8, issue #1089). Agent Substrate is a pinned
// cluster prerequisite that Koinon's installer step sets up, not part of the chart: its namespaces
// are fixed, so it is a cluster singleton. These checks read what the installer left, and run two
// short-lived pods in Substrate's namespace: the service-account issuer probe and the node device
// probe. They replace the OpenShell and Agent Sandbox checks when Config.Substrate is set.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/stateracloud/koinon/internal/project"
)

// Substrate prerequisites.
const (
	// SubstrateMinKubernetesMinor is the lowest Kubernetes 1.x minor the Substrate driver supports (D8):
	// certificates.k8s.io/v1 PodCertificateRequest and ClusterTrustBundle are GA there, so no beta gate.
	SubstrateMinKubernetesMinor = 37

	// SubstrateCRDGroup is the API group of Substrate's objects.
	SubstrateCRDGroup = "ate.dev"

	// SubstrateRouterSPIFFEFlag is the router flag that lets atenet's client reach the router (phase 0
	// proof P-B: without it, gateway-only egress does not work).
	SubstrateRouterSPIFFEFlag = "--ingress-allowed-spiffe-ids"

	// SubstrateIssuerProbeMarker starts the issuer probe's output line.
	SubstrateIssuerProbeMarker = "koinon-issuer-probe"
	// SubstrateDevicesProbeMarker starts the device probe's output line.
	SubstrateDevicesProbeMarker = "koinon-devices-probe"

	// substrateAPIServerContainer is the container of the api server Deployment that carries the release tag.
	substrateAPIServerContainer = "ate-api-server"
	substrateInstallDoc         = "install the pinned Substrate with the Koinon installer step (koinon-api substrate install)"
)

// SubstrateRouterSPIFFEID is the identity the router flag must allow: the atenet client service account
// in Substrate's namespace (Substrate v1.0.1 docs/get-started/quickstart.md).
func SubstrateRouterSPIFFEID(namespace string) string {
	return "spiffe://cluster.local/ns/" + namespace + "/sa/atenet-client"
}

// SubstrateAgentdSPIFFEID is the identity of agentd's pod certificate: the router allow-list must hold it, or
// every Exec of a run fails at the router handshake (issue #1446, ADR 0120 amendment). The pod identity signer
// issues spiffe://cluster.local/ns/<namespace>/sa/<service account>.
func SubstrateAgentdSPIFFEID(namespace, serviceAccount string) string {
	return "spiffe://cluster.local/ns/" + namespace + "/sa/" + serviceAccount
}

// SubstrateCRDs are the CRDs the preflight requires (plural.group), verified against the Substrate
// v1.0.1 manifests and the koinon-lab install.
var SubstrateCRDs = []string{
	"sandboxconfigs." + SubstrateCRDGroup,
	"workerpools." + SubstrateCRDGroup,
	"csidriverconfigs." + SubstrateCRDGroup,
}

// ErrDevicesMissing marks a device probe that did not start because a node lacks a device: the kubelet
// refused the hostPath mount (FailedMount). Any other probe error is not a device finding.
var ErrDevicesMissing = errors.New("a node lacks a microVM device")

// SubstrateDevices are the character devices a `vm` tier node needs: KVM, the TUN device for the
// guest network, and the vhost devices (their modules are vhost_vsock and vhost_net).
var SubstrateDevices = []string{"/dev/kvm", "/dev/net/tun", "/dev/vhost-vsock", "/dev/vhost-net"}

// SubstrateConfig is what the Substrate driver expects of the cluster.
type SubstrateConfig struct {
	// Namespace is Substrate's fixed namespace (ate-system).
	Namespace string
	// WorkerNamespace holds the WorkerPools (execution.substrate.workerNamespace, ate-workers).
	WorkerNamespace string
	// ProbeNamespace is where the probe pods run (default koinon-substrate-probe). It is never one of
	// Substrate's namespaces: their admission policies refuse a bare pod (CheckProbeNamespace).
	ProbeNamespace string
	// APIServerDeployment carries the release version in its image tag.
	APIServerDeployment string
	// RouterDeployment runs the router whose SPIFFE allow-list is checked.
	RouterDeployment string
	// RouterService is the Service of the router (default atenet-router). With RouterPort it is checked.
	RouterService string
	// RouterPort is the port of execution.substrate.routerAddress. When set, the router Service must serve it:
	// a wrong port fails every Exec, and this check finds it in seconds. 0 skips the check.
	RouterPort int
	// AgentdNamespace and AgentdServiceAccount name the ServiceAccount of Koinon's agentd, the only Koinon
	// component with a router identity. When the service account is set, the router allow-list must hold its
	// SPIFFE ID. Empty skips the check.
	AgentdNamespace      string
	AgentdServiceAccount string
	// WorkerPools maps a tier ("vm", "hardened") to the WorkerPool that must be ready. Empty skips.
	WorkerPools map[string]string
	// NodeSelector limits the device probe to the nodes that run workers.
	NodeSelector map[string]string
	// VMTier is true when the vm tier is in use: the nodes need the microVM devices.
	VMTier bool
	// GVisor is true when the gVisor sandbox class is in use: the nodes need user namespaces. NodeSelector
	// picks the nodes that are probed.
	GVisor bool
	// ExpectedIssuer is the issuer the installer passes to ate-setup (--ateapi-expected-jwt-issuer). Empty
	// means ate-setup reads the issuer from the API server's discovery document, as the probe does.
	ExpectedIssuer string
}

// WithDefaults fills the fixed names of Substrate v1.0.1.
func (s SubstrateConfig) WithDefaults() SubstrateConfig {
	if s.Namespace == "" {
		s.Namespace = "ate-system"
	}
	if s.WorkerNamespace == "" {
		s.WorkerNamespace = "ate-workers"
	}
	if s.ProbeNamespace == "" {
		s.ProbeNamespace = DefaultSubstrateProbeNamespace
	}
	if s.APIServerDeployment == "" {
		s.APIServerDeployment = "ate-api-server"
	}
	if s.RouterDeployment == "" {
		s.RouterDeployment = "atenet-router"
	}
	if s.RouterService == "" {
		s.RouterService = "atenet-router"
	}
	return s
}

// IssuerResolver returns the service-account token issuer URL of the cluster.
type IssuerResolver interface {
	Issuer(ctx context.Context) (string, error)
}

// IssuerResult is what an issuer probe pod saw.
type IssuerResult struct {
	// Reachable is true when the discovery document and the JWKS answered 2xx over the path Substrate's
	// api server takes (see ProbeIssuerFromPod) and the document names the probed URL as its issuer.
	Reachable bool
	// Unverified is true when that worked only over an unverified TLS connection: the pods do not
	// trust the issuer's certificate and Substrate's api server will refuse it.
	Unverified bool
	// TokenIssuer is the iss claim of the probe pod's own service-account token ("" when the pod had
	// none). Substrate's api server accepts a token only when its iss equals the configured issuer.
	TokenIssuer string
	Detail      string
}

// String formats the result as the probe prints it.
func (r IssuerResult) String() string {
	return fmt.Sprintf("%s reachable=%t unverified=%t tokenissuer=%q detail=%q", SubstrateIssuerProbeMarker, r.Reachable, r.Unverified, r.TokenIssuer, r.Detail)
}

// IssuerProber checks from a pod in Substrate's namespace that the issuer URL answers.
type IssuerProber interface {
	ProbeIssuer(ctx context.Context, issuer string) (IssuerResult, error)
}

// DevicesProber checks that a node has the microVM devices.
type DevicesProber interface {
	ProbeDevices(ctx context.Context) error
}

// kubeIssuerResolver reads the issuer from the API server's OpenID discovery document. The API
// server serves it to any authenticated client that has the system:service-account-issuer-discovery
// role (the preflight ClusterRole grants it).
type kubeIssuerResolver struct{ client kubernetes.Interface }

// NewIssuerResolver reads the issuer from the API server's /.well-known/openid-configuration.
func NewIssuerResolver(client kubernetes.Interface) IssuerResolver { return kubeIssuerResolver{client} }

func (r kubeIssuerResolver) Issuer(ctx context.Context) (string, error) {
	raw, err := r.client.Discovery().RESTClient().Get().AbsPath("/.well-known/openid-configuration").DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return ParseIssuer(raw)
}

// ParseIssuer reads "issuer" from an OpenID discovery document.
func ParseIssuer(doc []byte) (string, error) {
	var d struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return "", fmt.Errorf("parse discovery document: %w", err)
	}
	if d.Issuer == "" {
		return "", errors.New("discovery document has no issuer")
	}
	return d.Issuer, nil
}

// UnroutableIssuerHost says why a pod cannot reach the issuer host, or "" when the host looks
// routable. It reads only the URL: the probe pod gives the real answer. An IPv6 unique-local
// address (fc00::/7) is what Talos puts in the issuer (lab finding of #1089).
func UnroutableIssuerHost(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Hostname() == "" {
		return "the issuer is not a URL"
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return ""
	}
	switch {
	case addr.IsLoopback():
		return "a loopback address"
	case addr.IsLinkLocalUnicast():
		return "a link-local address"
	case addr.Is6() && addr.IsPrivate():
		return "an IPv6 unique-local address (fc00::/7)"
	}
	return ""
}

func (c *Checker) runSubstrate(ctx context.Context) []Result {
	netpol := make(chan Result, 1)
	go func() { netpol <- c.checkNetworkPolicy(ctx) }()
	c.defaultSubstrate()
	results := c.SubstratePrerequisites(ctx)
	results = append(results, c.checkSubstrate(ctx), c.checkSubstrateRouter(ctx))
	results = append(results, c.checkWorkerPools(ctx)...)
	if c.Config.PostgresMode == project.PostgresCNPG {
		results = append(results, c.checkCNPG(ctx))
	}
	if c.Config.GatewayMode == project.GatewayKubernetes {
		results = append(results, c.checkGatewayClass(ctx))
	}
	return append(results, <-netpol)
}

func (c *Checker) defaultSubstrate() {
	c.Config.Substrate = ptr.To(c.Config.SubstrateOrDefault())
}

// SubstrateOrDefault is the Substrate configuration with Substrate's fixed names filled in; the zero
// value when the driver is not substrate.
func (c Config) SubstrateOrDefault() SubstrateConfig {
	var sub SubstrateConfig
	if c.Substrate != nil {
		sub = *c.Substrate
	}
	return sub.WithDefaults()
}

// SubstratePrerequisites runs the checks that do not need Substrate: the Kubernetes version, the node
// devices of the vm tier and the service-account issuer.
func (c *Checker) SubstratePrerequisites(ctx context.Context) []Result {
	return append(c.SubstrateStaticPrerequisites(ctx), c.SubstrateProbePrerequisites(ctx)...)
}

// SubstrateStaticPrerequisites runs the prerequisite checks that need no namespace and no pod: the
// Kubernetes version and whether the service-account issuer can be read. The installer runs them first,
// so a cluster that cannot run Substrate fails before it creates anything.
func (c *Checker) SubstrateStaticPrerequisites(ctx context.Context) []Result {
	c.defaultSubstrate()
	results := []Result{c.checkKubernetesVersion(ctx)}
	if c.Issuer != nil {
		if _, _, err := c.substrateIssuer(ctx); err != nil {
			results = append(results, Result{
				Check: "Service-account issuer", Status: Fail,
				Message: fmt.Sprintf("cannot read the issuer from the API server's discovery document: %v", err),
				Fix:     "grant the preflight ServiceAccount the system:service-account-issuer-discovery ClusterRole",
			})
		}
	}
	return results
}

// SubstrateProbePrerequisites runs the prerequisite checks that start pods in the probe namespace: the
// node devices of the vm tier, the user namespaces of the gVisor nodes and the service-account issuer probe.
func (c *Checker) SubstrateProbePrerequisites(ctx context.Context) []Result {
	c.defaultSubstrate()
	var results []Result
	if c.Config.Substrate.VMTier {
		results = append(results, c.checkSubstrateDevices(ctx))
	}
	results = append(results, c.checkUserNamespaces(ctx), c.checkAteFS(ctx))
	return append(results, c.checkIssuer(ctx))
}

// parseMinor reads the minor of a Kubernetes version.Info: "37", or "37+" on managed clusters.
func parseMinor(minor string) (int, error) {
	return strconv.Atoi(strings.TrimRight(strings.TrimSpace(minor), "+"))
}

func (c *Checker) checkKubernetesVersion(ctx context.Context) Result {
	const check = "Kubernetes version"
	type versionResult struct {
		major, minor, git string
		err               error
	}
	ch := make(chan versionResult, 1)
	go func() {
		v, err := c.Kube.Discovery().ServerVersion()
		if err != nil {
			ch <- versionResult{err: err}
			return
		}
		ch <- versionResult{major: v.Major, minor: v.Minor, git: v.GitVersion}
	}()
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	var r versionResult
	select {
	case r = <-ch:
	case <-ctx.Done():
		return Result{Check: check, Status: Fail, Message: "reading the server version timed out"}
	}
	if r.err != nil {
		return Result{Check: check, Status: Fail, Message: fmt.Sprintf("reading the server version: %v", r.err)}
	}
	minor, err := parseMinor(r.minor)
	if err != nil || strings.TrimSpace(r.major) != "1" {
		return Result{Check: check, Status: Fail, Message: fmt.Sprintf("cannot read the Kubernetes version %q.%q", r.major, r.minor)}
	}
	if minor < SubstrateMinKubernetesMinor {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("Kubernetes %s is below 1.%d: the Substrate driver needs the GA PodCertificateRequest and ClusterTrustBundle APIs (ADR 0119 D8)", r.git, SubstrateMinKubernetesMinor),
			Fix:     fmt.Sprintf("upgrade the cluster to Kubernetes 1.%d or later, or keep execution.driver: openshell", SubstrateMinKubernetesMinor),
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("Kubernetes %s", r.git)}
}

func (c *Checker) checkSubstrate(ctx context.Context) Result {
	const check = "Agent Substrate"
	sub := c.Config.Substrate
	var missing []string
	for _, name := range SubstrateCRDs {
		if _, err := c.getCRD(ctx, name); apierrors.IsNotFound(err) {
			missing = append(missing, name)
		} else if err != nil {
			return apiFailure(check, "CRD "+name, err)
		}
	}
	if len(missing) > 0 {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRDs not found: %s. Substrate is not installed", strings.Join(missing, ", ")),
			Fix:     substrateInstallDoc,
		}
	}
	dctx, cancel := c.callCtx(ctx)
	defer cancel()
	deploy, err := c.Kube.AppsV1().Deployments(sub.Namespace).Get(dctx, sub.APIServerDeployment, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("CRDs present but Deployment %s/%s not found", sub.Namespace, sub.APIServerDeployment),
			Fix:     substrateInstallDoc,
		}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("Deployment %s/%s", sub.Namespace, sub.APIServerDeployment), err)
	}
	if deploy.Status.ReadyReplicas < 1 {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("Deployment %s/%s has no ready replica", sub.Namespace, sub.APIServerDeployment),
			Fix:     "check the pods and logs in namespace " + sub.Namespace,
		}
	}
	var image string
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		if ctr.Name == substrateAPIServerContainer {
			image = ctr.Image
		}
	}
	tag := imageTag(image)
	switch {
	case tag == "":
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("container %s of %s/%s has image %q: no tag, so its version is unknown", substrateAPIServerContainer, sub.Namespace, sub.APIServerDeployment, image),
			Fix:     "deploy the api server from a tagged release image: " + substrateInstallDoc,
		}
	case tag != SubstratePinnedVersion:
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("api server version %s is not the pinned release %s (ADR 0119 D4)", tag, SubstratePinnedVersion),
			Fix:     substrateInstallDoc,
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("CRDs present; api server %s ready", tag)}
}

func (c *Checker) checkSubstrateRouter(ctx context.Context) Result {
	const check = "Substrate router"
	sub := c.Config.Substrate
	dctx, cancel := c.callCtx(ctx)
	defer cancel()
	deploy, err := c.Kube.AppsV1().Deployments(sub.Namespace).Get(dctx, sub.RouterDeployment, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("Deployment %s/%s not found", sub.Namespace, sub.RouterDeployment),
			Fix:     substrateInstallDoc,
		}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("Deployment %s/%s", sub.Namespace, sub.RouterDeployment), err)
	}
	want := []string{SubstrateRouterSPIFFEID(sub.Namespace)}
	if sub.AgentdServiceAccount != "" {
		want = append(want, SubstrateAgentdSPIFFEID(sub.AgentdNamespace, sub.AgentdServiceAccount))
	}
	allowed := routerAllowedIDs(deploy)
	for _, id := range want {
		if slices.Contains(allowed, id) {
			continue
		}
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("router does not allow %s: actors cannot reach the gateway through atenet (proof P-B), or agentd cannot reach the actors (every Exec fails)", id),
			Fix:     fmt.Sprintf("start the router with %s=%s (the Koinon installer step sets it)", SubstrateRouterSPIFFEFlag, strings.Join(want, ",")),
		}
	}
	if sub.RouterPort > 0 {
		if r := c.checkRouterPort(ctx, check, *sub); r.Status != Pass {
			return r
		}
	}
	return Result{Check: check, Status: Pass, Message: "router allows " + strings.Join(want, ", ")}
}

// routerAllowedIDs are the SPIFFE IDs of the router's allow-list flag, in its command and arguments.
func routerAllowedIDs(deploy *appsv1.Deployment) []string {
	var ids []string
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		for _, arg := range slices.Concat(ctr.Command, ctr.Args) {
			if v, ok := strings.CutPrefix(arg, SubstrateRouterSPIFFEFlag+"="); ok {
				ids = append(ids, strings.Split(v, ",")...)
			}
		}
	}
	return ids
}

// checkRouterPort checks that the router Service serves the port of routerAddress. The router has no plain port:
// Substrate v1.0.1 serves TLS on Service port 443 (container port 8443), so a leftover :80 fails here and not at
// the first Exec.
func (c *Checker) checkRouterPort(ctx context.Context, check string, sub SubstrateConfig) Result {
	dctx, cancel := c.callCtx(ctx)
	defer cancel()
	svc, err := c.Kube.CoreV1().Services(sub.Namespace).Get(dctx, sub.RouterService, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{Check: check, Status: Fail, Message: fmt.Sprintf("Service %s/%s not found", sub.Namespace, sub.RouterService), Fix: substrateInstallDoc}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("Service %s/%s", sub.Namespace, sub.RouterService), err)
	}
	ports := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		if int(p.Port) == sub.RouterPort {
			return Result{Check: check, Status: Pass}
		}
		ports = append(ports, strconv.Itoa(int(p.Port)))
	}
	return Result{
		Check: check, Status: Fail,
		Message: fmt.Sprintf("Service %s/%s has no port %d (it has %s): every Exec would fail to connect", sub.Namespace, sub.RouterService, sub.RouterPort, strings.Join(ports, ", ")),
		Fix:     fmt.Sprintf("set execution.substrate.routerAddress to the router Service port (%s)", strings.Join(ports, " or ")),
	}
}

func (c *Checker) checkWorkerPools(ctx context.Context) []Result {
	sub := c.Config.Substrate
	tiers := make([]string, 0, len(sub.WorkerPools))
	for t := range sub.WorkerPools {
		tiers = append(tiers, t)
	}
	slices.Sort(tiers)
	crd, err := c.getCRD(ctx, "workerpools."+SubstrateCRDGroup)
	var results []Result
	for _, tier := range tiers {
		check := fmt.Sprintf("Substrate worker pool (%s tier)", tier)
		if err != nil {
			results = append(results, Result{Check: check, Status: Fail, Message: fmt.Sprintf("reading the WorkerPool CRD: %v", err), Fix: substrateInstallDoc})
			continue
		}
		results = append(results, c.checkWorkerPool(ctx, check, sub.WorkerNamespace, sub.WorkerPools[tier], crd))
	}
	return results
}

func (c *Checker) checkWorkerPool(ctx context.Context, check, namespace, name string, crd *unstructured.Unstructured) Result {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	var version string
	for _, v := range versions {
		if m, ok := v.(map[string]any); ok {
			if s, _, _ := unstructured.NestedBool(m, "storage"); s {
				version, _, _ = unstructured.NestedString(m, "name")
			}
		}
	}
	gvr := schema.GroupVersionResource{Group: SubstrateCRDGroup, Version: version, Resource: "workerpools"}
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	pool, err := c.Dynamic.Resource(gvr).Namespace(namespace).Get(cctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("WorkerPool %s/%s not found", namespace, name),
			Fix:     "the chart creates the default pool (execution.substrate.workerPool); enable it, or set the pool name of this tier in the chart values",
		}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("WorkerPool %s/%s", namespace, name), err)
	}
	// Substrate v1.0.1 has no top-level conditions on a WorkerPool: status carries replicas and
	// readyReplicas, and each sandbox class a Resolved condition (api/infra/k8s/ate/v1).
	want, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	have, found, _ := unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
	if !found || have < 1 || have < want {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("WorkerPool %s/%s has %d of %d workers ready", namespace, name, have, want),
			Fix:     "check the worker pods and the atelet logs in namespace " + namespace,
		}
	}
	classes, _, _ := unstructured.NestedSlice(pool.Object, "status", "sandboxClasses")
	for _, class := range classes {
		m, _ := class.(map[string]any)
		conds, _, _ := unstructured.NestedSlice(m, "conditions")
		for _, cond := range conds {
			if cm, ok := cond.(map[string]any); ok && cm["type"] == "Resolved" && cm["status"] != "True" {
				return Result{
					Check: check, Status: Fail,
					Message: fmt.Sprintf("WorkerPool %s/%s: sandbox class %v does not resolve: %v", namespace, name, m["name"], cm["message"]),
					Fix:     "check the SandboxConfig the class names (kubectl get sandboxconfigs.ate.dev)",
				}
			}
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("WorkerPool %s/%s ready (%d of %d workers)", namespace, name, have, want)}
}

func (c *Checker) checkSubstrateDevices(ctx context.Context) Result {
	const check = "Substrate node devices"
	if c.Devices == nil {
		return Result{Check: check, Status: Warn, Message: "the device probe is disabled; /dev/kvm and the other microVM devices are not checked"}
	}
	if err := c.Devices.ProbeDevices(ctx); err != nil {
		if !errors.Is(err, ErrDevicesMissing) {
			return Result{
				Check: check, Status: Fail,
				Message: fmt.Sprintf("the device probe pod could not run in namespace %s: %v", c.Config.Substrate.ProbeNamespace, err),
				Fix:     "this is not a device problem: fix the cause above (the namespace, Pod Security, image pull or scheduling), then run the check again",
			}
		}
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("a node lacks one of %s: %v", strings.Join(SubstrateDevices, ", "), err),
			Fix: "run the vm tier on nodes with KVM (nested virtualisation on cloud VMs), load the modules vhost_vsock and vhost_net, " +
				"and tun; or use the hardened tier (gVisor), which needs no KVM",
		}
	}
	return Result{Check: check, Status: Pass, Message: "one node mounted " + strings.Join(SubstrateDevices, ", ") + " (only that node is probed)"}
}

// SubstrateIssuer is the issuer Substrate's api server will trust: the configured override when set, else
// the issuer of the API server's discovery document. It fails only when it needs the discovery document and
// cannot read it.
func (c *Checker) SubstrateIssuer(ctx context.Context) (string, error) {
	c.defaultSubstrate()
	issuer, _, err := c.substrateIssuer(ctx)
	return issuer, err
}

// substrateIssuer is SubstrateIssuer with a note for the check message: what the discovery document names
// when the override differs from it, or why it could not be read.
func (c *Checker) substrateIssuer(ctx context.Context) (issuer, info string, err error) {
	rctx, cancel := c.callCtx(ctx)
	discovered, err := c.Issuer.Issuer(rctx)
	cancel()
	exp := c.Config.Substrate.ExpectedIssuer
	switch {
	case exp == "":
		return discovered, "", err
	case err != nil:
		return exp, fmt.Sprintf(" (the API server's discovery document could not be read: %s)", truncate(err.Error(), 150)), nil
	case discovered != exp:
		return exp, fmt.Sprintf(" (the API server's discovery document names %s)", discovered), nil
	}
	return exp, "", nil
}

func (c *Checker) checkIssuer(ctx context.Context) Result {
	const check = "Service-account issuer"
	if c.Issuer == nil || c.IssuerProbe == nil {
		return Result{Check: check, Status: Warn, Message: "the issuer probe is disabled; whether Substrate can validate service-account tokens is not checked"}
	}
	// The issuer Substrate's api server trusts is the one ate-setup writes: --ateapi-expected-jwt-issuer when
	// given, else the discovery document's issuer (cmd/ate-setup/internal/steps/create.go jwtIssuer). With
	// the override ate-setup does not read the discovery document, so here it is only information.
	issuer, info, err := c.substrateIssuer(ctx)
	if err != nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("cannot read the issuer from the API server's discovery document: %v", err),
			Fix:     "grant the preflight ServiceAccount the system:service-account-issuer-discovery ClusterRole",
		}
	}
	// The probe pod runs in the probe namespace (issue #1176), not in Substrate's namespace: the issuer and the iss
	// of a token are the same in every namespace. ns is Substrate's namespace, which the fixes name.
	ns, probeNS := c.Config.Substrate.Namespace, c.Config.Substrate.ProbeNamespace
	res, err := c.IssuerProbe.ProbeIssuer(ctx, issuer)
	if err != nil {
		return Result{Check: check, Status: Fail, Message: fmt.Sprintf("issuer probe pod: %v", err)}
	}
	tokenInfo := ""
	if res.TokenIssuer != "" {
		tokenInfo = fmt.Sprintf("; the tokens of pods carry iss %s", res.TokenIssuer)
	}
	if res.Reachable && res.Unverified {
		fix := "serve the issuer with a certificate signed by a CA in the system roots of the Substrate image (Substrate trusts only those for an issuer outside the cluster)"
		if slices.Contains(inClusterIssuers, issuer) {
			fix = "for the in-cluster issuer Substrate trusts only the pod's mounted " + IssuerCAFile + ": check that this is the cluster CA " +
				"and that the API server's serving certificate is signed by it and names " + strings.TrimPrefix(issuer, "https://")
		}
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("a pod in %s reaches %s, but only without verifying its TLS certificate (%s); Substrate's api server verifies it and would fail with \"invalid bearer token\"", probeNS, issuer, res.Detail),
			Fix:     fix,
		}
	}
	if !res.Reachable {
		msg := fmt.Sprintf("Substrate's api server could not validate service-account tokens of issuer %s from a pod in namespace %s (%s). "+
			"It fetches <issuer>/.well-known/openid-configuration and the document's jwks_uri, so every call would fail with \"invalid bearer token\"",
			issuer, probeNS, res.Detail)
		if why := UnroutableIssuerHost(issuer); why != "" {
			msg += ". The issuer host is " + why
		}
		return Result{
			Check: check, Status: Fail, Message: msg + info + tokenInfo,
			Fix: "pods in " + ns + " must reach the issuer Substrate trusts, and Substrate v1.0.1 has no separate discovery URL. " +
				"Set the API server's first --service-account-issuer to https://kubernetes.default.svc (Substrate then reads discovery from the in-cluster API server with the pod's token), " +
				"or to a URL that pods reach with a certificate from the system roots (on Talos: cluster.apiServer.extraArgs service-account-issuer). " +
				"Keep the old value as a second --service-account-issuer so tokens already issued stay valid, and when the discovery document's jwks_uri is not reachable from pods, " +
				"set --service-account-jwks-uri to https://kubernetes.default.svc/openid/v1/jwks. " +
				"Kubernetes signs tokens and names the discovery issuer from that first value, so --expected-jwt-issuer cannot replace it. A sidecar that fakes the issuer is not a supported install" +
				SafeIssuerPatch(res.TokenIssuer, issuer),
		}
	}
	if res.TokenIssuer == "" {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("a pod in %s fetches the discovery document and JWKS of %s%s, but the probe could not read the iss claim of its service-account token, "+
				"so it cannot tell whether Substrate would accept the tokens (Substrate rejects a token whose iss differs from the issuer)", probeNS, issuer, info),
			Fix: "run the probe with a koinon-api image of this release (an older image does not report the token issuer), and check that pods in " + ns + " get a projected service-account token",
		}
	}
	if res.TokenIssuer != issuer {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("Substrate would trust issuer %s%s, but the service-account tokens of pods in %s carry iss %s: its api server rejects every token with \"unexpected issuer\"", issuer, info, probeNS, res.TokenIssuer),
			Fix: "Substrate must trust the iss the tokens carry (" + res.TokenIssuer + "), and pods in " + ns + " must reach that URL: Substrate v1.0.1 fetches discovery from the issuer itself. " +
				"Drop --expected-jwt-issuer (or set it to " + res.TokenIssuer + ") and run the check again: it then probes that URL. " +
				"If pods cannot reach it, set the API server's first --service-account-issuer to https://kubernetes.default.svc or another URL pods reach " +
				"(Kubernetes signs tokens with the first value; later ones are only accepted for validation)" +
				SafeIssuerPatch(res.TokenIssuer, res.TokenIssuer),
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("a pod in %s fetches the discovery document and JWKS of %s%s%s", probeNS, issuer, info, tokenInfo)}
}

// SafeIssuerPatch is the cluster change that makes the in-cluster issuer usable without invalidating a
// token that exists: the new issuer signs, the old issuer stays accepted, and both audiences stay accepted.
// It names the Talos and Omni form and the kubeadm form, and the cost. oldIssuer is the issuer the cluster
// uses now (the iss of its tokens when known, else the discovery document's issuer: pass both, the first
// non-empty one is used). It returns "" when the cluster already uses the in-cluster issuer, as then the
// change would do nothing. It never prints a patch that sets the issuer alone: that would make the API
// server refuse every token that carries the old audience.
//
// Prior art: Sidero documents this patch for Substrate on Omni; Talos' APIAudiences is accepted.audiences
// when set, else the issuer URL (internal/app/machined/pkg/controllers/k8s/service_account.go); Omni refuses
// only issuer.privateKey in this document.
func SafeIssuerPatch(oldIssuer ...string) string {
	old := ""
	for _, o := range oldIssuer {
		if o != "" {
			old = o
			break
		}
	}
	const in = "https://kubernetes.default.svc"
	if old == "" || slices.Contains(inClusterIssuers, old) {
		return ""
	}
	return fmt.Sprintf(`. The safe change keeps the old issuer and both audiences, so tokens that exist stay valid, and costs one API server restart.
Before you apply it, read the audiences the cluster accepts now (--api-audiences, or accepted.audiences) and keep every entry in the list: the lists below show only the two issuers. Dropping one, such as an audience another system uses, makes the API server refuse its tokens.
Talos and Omni (a cluster config patch):
---
apiVersion: v1alpha1
kind: KubeServiceAccountConfig
issuer:
  issuerURL: %[1]s
accepted:
  issuers:
    - %[2]s
  audiences:
    - %[1]s
    - %[2]s
---
kubeadm (the kube-apiserver flags; the first --service-account-issuer signs):
  --service-account-issuer=%[1]s
  --service-account-issuer=%[2]s
  --api-audiences=%[1]s,%[2]s
Do not set the new issuer alone: the API server then rejects every token with the old audience. Keep any other audience the cluster accepts now`, in, old)
}

// issuerDiscoveryPath is the discovery path the probe fetches under the issuer.
const issuerDiscoveryPath = ".well-known/openid-configuration"

// Where a pod finds the cluster CA and its token. Substrate's ate-setup points ate-api-server at both when the
// issuer is the in-cluster one.
const (
	IssuerCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	IssuerTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101: a path, not a credential
)

// inClusterIssuers are the issuers for which ate-setup configures ate-api-server with the pod's own CA and
// token (kubernetesJWTProvider in cmd/ate-setup/internal/steps/create.go).
var inClusterIssuers = []string{"https://kubernetes.default.svc", "https://kubernetes.default.svc.cluster.local"}

// ProbeIssuerFromPod is what the issuer probe pod runs. It repeats what Substrate v1.0.1's api server does
// to validate a service-account token, and nothing else (permalinks at the pinned commit
// 20778a6dbb01f13024642851bb592bf67c9fec05):
//   - internal/oidcjwt/oidcjwt.go discoverKeysForIssuer: GET <issuer>/.well-known/openid-configuration, require
//     its "issuer" to equal the configured issuer, then GET its jwks_uri. The issuer string itself is not
//     dialled for anything else; the JWKS comes from wherever jwks_uri points.
//   - cmd/ate-setup/internal/steps/create.go kubernetesJWTProvider: for the in-cluster issuer
//     (https://kubernetes.default.svc) the provider carries certificateAuthorityFile and discoveryTokenFile,
//     the pod's own serviceaccount ca.crt and token. For any other issuer it has neither: the system roots
//     verify the server and no credential is sent.
//   - internal/oidcjwt/http.go issuerDiscoveryTransport: the token goes on requests under the issuer and on
//     https requests to the path /openid/v1/jwks (Kubernetes' JWKS path, so a jwks_uri on the API server's
//     own address works).
//   - internal/oidcjwt/oidcjwt.go Verify: a token is accepted only when its iss claim equals the issuer.
//
// So the in-cluster issuer needs no route outside the cluster, but only when the cluster itself issues tokens
// with it (its first --service-account-issuer): the discovery document must name it and the tokens must carry
// it. Naming it in Substrate alone, on a cluster whose own issuer is an address pods cannot reach (an Omni or
// Talos IPv6 address), fails both checks. The probe reports the iss claim of its own token so the caller can
// compare it. The issuer counts as reachable only
// when both fetches answer HTTP 200 and the JWKS holds a key Substrate can use. When only an unverified
// connection works, the result is reachable and Unverified. The HTTP status goes into Detail.
func ProbeIssuerFromPod(ctx context.Context, issuer string, timeout time.Duration) IssuerResult {
	return ProbeIssuerWith(ctx, issuer, timeout, IssuerProbeFiles{
		CAFile: IssuerCAFile, TokenFile: IssuerTokenFile,
		// An exact match, as ate-setup's switch: a trailing slash gets neither the CA nor the token there.
		InCluster: slices.Contains(inClusterIssuers, issuer),
	})
}

// IssuerProbeFiles tells ProbeIssuerWith where the pod's credentials are and whether Substrate would use them.
type IssuerProbeFiles struct {
	// CAFile and TokenFile are the pod's serviceaccount CA and token.
	CAFile, TokenFile string
	// InCluster is true for the in-cluster issuer: Substrate then trusts CAFile alone and sends TokenFile.
	InCluster bool
}

// ProbeIssuerWith is ProbeIssuerFromPod with the credential files given.
func ProbeIssuerWith(ctx context.Context, issuer string, timeout time.Duration, files IssuerProbeFiles) IssuerResult {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return IssuerResult{Detail: "not a URL"}
	}
	tokenIssuer := tokenIssuerFromFile(files.TokenFile)
	res := fetchIssuer(ctx, issuer, timeout, files, true)
	if !res.Reachable && (strings.Contains(res.Detail, "certificate") || strings.Contains(res.Detail, "x509")) {
		// The diagnostic retry sends no token: the server it reaches is not authenticated. An in-cluster issuer
		// that needs the token then answers 401, which still proves only that it is unreachable when verified.
		if unv := fetchIssuer(ctx, issuer, timeout, files, false); unv.Reachable {
			unv.Unverified = true
			unv.Detail = "TLS verification failed: " + truncate(res.Detail, 150)
			res = unv
		}
	}
	res.TokenIssuer = tokenIssuer
	return res
}

// tokenIssuerFromFile reads the iss claim of a JWT file without verifying it: the probe only compares the
// string. It returns "" when the file or the claim is missing.
func tokenIssuerFromFile(file string) string {
	raw, err := os.ReadFile(file) //nolint:gosec // G304: the pod's own token path
	if err != nil {
		return ""
	}
	return jwtIssuer(strings.TrimSpace(string(raw)))
}

// jwtIssuer is the unverified iss claim of a compact JWT, or "".
func jwtIssuer(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Issuer
}

// issuerScoped says whether Substrate sends its discovery token on a request to target: under the issuer, or
// the Kubernetes JWKS path (internal/oidcjwt/http.go issuerScopedURL and isKubernetesJWKSURL).
func issuerScoped(target, issuer string) bool {
	t, err := url.Parse(target)
	i, err2 := url.Parse(issuer)
	if err != nil || err2 != nil {
		return false
	}
	if strings.EqualFold(t.Scheme, "https") && t.EscapedPath() == "/openid/v1/jwks" {
		return true
	}
	if !strings.EqualFold(t.Scheme, i.Scheme) || !strings.EqualFold(t.Host, i.Host) {
		return false
	}
	ip := strings.TrimRight(i.EscapedPath(), "/")
	return ip == "" || t.EscapedPath() == ip || strings.HasPrefix(t.EscapedPath(), ip+"/")
}

// issuerTokenTransport adds the pod's token to each request that issuerScoped allows, as Substrate's
// issuerDiscoveryTransport does. It decides per request, so a redirect gets the token only when its own URL
// is in scope: the client never copies an Authorization header the first request did not carry.
type issuerTokenTransport struct {
	base              http.RoundTripper
	issuer, tokenFile string
}

func (t issuerTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !issuerScoped(req.URL.String(), t.issuer) {
		return t.base.RoundTrip(req)
	}
	tok, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the pod's service-account token: %w", err)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	return t.base.RoundTrip(req)
}

// fetchIssuer fetches discovery and JWKS as Substrate does. verify selects the TLS check Substrate makes;
// without it any certificate is accepted and no token is sent.
func fetchIssuer(ctx context.Context, issuer string, timeout time.Duration, files IssuerProbeFiles, verify bool) IssuerResult {
	var transport http.RoundTripper = &http.Transport{
		DialContext:     (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig: issuerTLS(files, verify),
	}
	if files.InCluster && verify {
		transport = issuerTokenTransport{base: transport, issuer: issuer, tokenFile: files.TokenFile}
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	get := func(target string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		if err != nil {
			return 0, nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, body, nil
	}
	docURL := issuer
	if !strings.HasSuffix(docURL, "/") {
		docURL += "/"
	}
	docURL += issuerDiscoveryPath
	code, body, err := get(docURL)
	if err != nil {
		return IssuerResult{Detail: truncate(err.Error(), 200)}
	}
	// Substrate's fetchJSON accepts HTTP 200 only.
	if code != http.StatusOK {
		return IssuerResult{Detail: fmt.Sprintf("discovery document: HTTP %d", code)}
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.JWKSURI == "" {
		return IssuerResult{Detail: "discovery document: HTTP " + strconv.Itoa(code) + " but no jwks_uri in the JSON"}
	}
	if doc.Issuer != issuer {
		return IssuerResult{Detail: fmt.Sprintf("discovery document: issuer %q differs from %q", truncate(doc.Issuer, 100), issuer)}
	}
	jcode, jbody, err := get(doc.JWKSURI)
	if err != nil {
		return IssuerResult{Detail: "jwks_uri " + truncate(doc.JWKSURI, 100) + ": " + truncate(err.Error(), 200)}
	}
	if jcode != http.StatusOK {
		return IssuerResult{Detail: fmt.Sprintf("jwks_uri %s: HTTP %d", truncate(doc.JWKSURI, 100), jcode)}
	}
	n, err := usableJWKs(jbody)
	if err != nil {
		return IssuerResult{Detail: fmt.Sprintf("jwks_uri %s: %v", truncate(doc.JWKSURI, 100), err)}
	}
	return IssuerResult{Reachable: true, Detail: fmt.Sprintf("discovery HTTP %d, jwks HTTP %d with %d usable keys", code, jcode, n)}
}

// usableJWKs counts the keys of a JWKS that Substrate's parseJWK accepts (internal/oidcjwt/oidcjwt.go): a kid
// and an RSA key with n and e, or an EC key on P-256, P-384 or P-521 with x and y. Substrate fails on a set
// without one. The probe checks the fields, not the key material.
func usableJWKs(body []byte) (int, error) {
	var set struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
			CRV string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return 0, errors.New("not a JSON JWKS")
	}
	n := 0
	for _, k := range set.Keys {
		switch {
		case k.KID == "":
		case k.KTY == "RSA" && k.N != "" && k.E != "":
			n++
		case k.KTY == "EC" && slices.Contains([]string{"P-256", "P-384", "P-521"}, k.CRV) && k.X != "" && k.Y != "":
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no usable key in the JWKS (%d keys)", len(set.Keys))
	}
	return n, nil
}

// ParseIssuerProbeOutput reads the probe line from the probe pod's log.
func ParseIssuerProbeOutput(log string) (IssuerResult, error) {
	for line := range strings.Lines(log) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), SubstrateIssuerProbeMarker+" ")
		if !ok {
			continue
		}
		var res IssuerResult
		flags, detail, _ := strings.Cut(rest, " detail=")
		for _, f := range strings.Fields(flags) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "reachable":
				if v != "true" && v != "false" {
					return IssuerResult{}, fmt.Errorf("malformed probe output %q", truncate(line, 200))
				}
				res.Reachable = v == "true"
			case "unverified":
				res.Unverified = v == "true"
			case "tokenissuer":
				res.TokenIssuer, _ = strconv.Unquote(v)
			}
		}
		if !strings.Contains(flags, "reachable=") {
			return IssuerResult{}, fmt.Errorf("malformed probe output %q", truncate(line, 200))
		}
		res.Detail, _ = strconv.Unquote(detail)
		return res, nil
	}
	return IssuerResult{}, fmt.Errorf("no %q line in the probe pod's log: %q", SubstrateIssuerProbeMarker, truncate(log, 200))
}

// KubeSubstrateProber runs the issuer probe and the device probe as pods in the probe namespace
// (a privileged Pod Security namespace; the issuer pod is unprivileged and carries the default ServiceAccount token
// to probe with it). Namespace is never one of Substrate's namespaces: their admission policies refuse a bare pod.
type KubeSubstrateProber struct {
	Client           kubernetes.Interface
	Namespace        string
	NamePrefix       string
	Image            string
	ImagePullSecrets []string
	Labels           map[string]string
	// NodeSelector places the device probe on the worker nodes and picks the nodes of the user-namespace
	// probe. The issuer probe does not use it: it checks the network path of the cluster, not of the workers.
	NodeSelector map[string]string
	Timeout      time.Duration
	PollInterval time.Duration
	// Logs reads a pod's log; nil reads it through Client.
	Logs func(ctx context.Context, namespace, name string) (string, error)
}

func (p KubeSubstrateProber) timeout() time.Duration { return orDuration(p.Timeout, 90*time.Second) }

func (p KubeSubstrateProber) pod(component string, args []string, volumes []corev1.Volume, mounts []corev1.VolumeMount) *corev1.Pod {
	var pullSecrets []corev1.LocalObjectReference
	for _, s := range p.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s})
	}
	labels := map[string]string{}
	for k, v := range p.Labels {
		labels[k] = v
	}
	labels["app.kubernetes.io/component"] = component
	deadline := int64(p.timeout().Seconds())
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: p.NamePrefix + "-" + component + "-", Namespace: p.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			ActiveDeadlineSeconds:        &deadline,
			ImagePullSecrets:             pullSecrets,
			Volumes:                      volumes,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65532),
				RunAsGroup:     ptr.To[int64](65532),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "probe", Image: p.Image, Args: args, VolumeMounts: mounts,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Privileged:               ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
		},
	}
}

// IssuerPod is the issuer probe pod. It mounts the default ServiceAccount's token and CA, as Substrate's
// api server does for the in-cluster issuer, and sends no other credential.
func (p KubeSubstrateProber) IssuerPod(issuer string) *corev1.Pod {
	pod := p.pod("issuer-probe", []string{"preflight", "issuer-probe", "--issuer", issuer}, nil, nil)
	pod.Spec.AutomountServiceAccountToken = ptr.To(true)
	return pod
}

// DevicesPod is the device probe pod. It mounts each device as a hostPath of type CharDevice, so the
// kubelet starts it only on a node where every one exists as a character device. It does not open them.
func (p KubeSubstrateProber) DevicesPod() *corev1.Pod {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	for i, dev := range SubstrateDevices {
		name := "dev" + strconv.Itoa(i)
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: dev, Type: ptr.To(corev1.HostPathCharDev),
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: "/probe" + dev, ReadOnly: true})
	}
	pod := p.pod("devices-probe", []string{"preflight", "devices-probe"}, volumes, mounts)
	pod.Spec.NodeSelector = p.NodeSelector
	return pod
}

// ProbeIssuer runs the issuer probe pod.
func (p KubeSubstrateProber) ProbeIssuer(ctx context.Context, issuer string) (IssuerResult, error) {
	log, err := p.run(ctx, p.IssuerPod(issuer))
	if err != nil {
		return IssuerResult{}, err
	}
	return ParseIssuerProbeOutput(log)
}

// ProbeDevices runs the device probe pod.
func (p KubeSubstrateProber) ProbeDevices(ctx context.Context) error {
	log, err := p.run(ctx, p.DevicesPod())
	var notStarted *podNotStartedError
	if errors.As(err, &notStarted) && notStarted.reason == "FailedMount" && namesDevice(notStarted.eventMessage) {
		return fmt.Errorf("%w: %w", ErrDevicesMissing, err)
	}
	if err != nil {
		return err
	}
	if !strings.Contains(log, SubstrateDevicesProbeMarker) {
		return fmt.Errorf("no %q line in the probe pod's log: %q", SubstrateDevicesProbeMarker, truncate(log, 200))
	}
	return nil
}

// run creates the pod, waits for it to finish, reads its log and deletes it. When the pod does not
// start, the error carries the kubelet's event (a missing device is a FailedMount).
func (p KubeSubstrateProber) run(ctx context.Context, want *corev1.Pod) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	pods := p.Client.CoreV1().Pods(p.Namespace)
	if err := p.deleteStale(ctx, want); err != nil {
		return "", err
	}
	pod, err := pods.Create(ctx, want, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create probe pod: %w", err)
	}
	name := pod.Name
	defer func() {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = pods.Delete(delCtx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	}()
	interval := orDuration(p.PollInterval, 2*time.Second)
	for pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		select {
		case <-ctx.Done():
			return "", p.notStarted(pod, fmt.Sprintf("still %s after %s", pod.Status.Phase, p.timeout()))
		case <-time.After(interval):
		}
		if pod, err = pods.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return "", fmt.Errorf("read probe pod %s: %w", name, err)
		}
	}
	// ActiveDeadlineSeconds can fail the pod before its container ever ran (a hostPath the kubelet refuses):
	// then there is no log, and the warning events tell why.
	if pod.Status.Phase == corev1.PodFailed && !containerRan(pod) {
		return "", p.notStarted(pod, fmt.Sprintf("failed before its container started (%s)", pod.Status.Reason))
	}
	readLogs := p.Logs
	if readLogs == nil {
		readLogs = p.podLogs
	}
	log, err := readLogs(ctx, p.Namespace, name)
	if err != nil {
		return "", fmt.Errorf("read probe pod %s log: %w", name, err)
	}
	return log, nil
}

// deleteStale deletes the pods an earlier run left behind (a killed installer never ran its cleanup):
// those with the component and the part-of label of the probe pod about to start. Without a part-of
// label the probe cannot tell its own pods from others, so it deletes nothing.
func (p KubeSubstrateProber) deleteStale(ctx context.Context, want *corev1.Pod) error {
	const component, partOf = "app.kubernetes.io/component", "app.kubernetes.io/part-of"
	if want.Labels[component] == "" || want.Labels[partOf] == "" {
		return nil
	}
	sel := labels.SelectorFromSet(labels.Set{component: want.Labels[component], partOf: want.Labels[partOf]})
	pods := p.Client.CoreV1().Pods(p.Namespace)
	stale, err := pods.List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return fmt.Errorf("list stale probe pods in %s: %w", p.Namespace, err)
	}
	for _, pod := range stale.Items {
		if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale probe pod %s/%s: %w", p.Namespace, pod.Name, err)
		}
	}
	return nil
}

// podNotStartedError is a probe pod whose container did not run to the end; reason and eventMessage are the
// Reason and the Message of its newest warning event.
type podNotStartedError struct{ reason, eventMessage, msg string }

func (e *podNotStartedError) Error() string { return e.msg }

// notStarted is the podNotStartedError of pod, with its newest warning event.
func (p KubeSubstrateProber) notStarted(pod *corev1.Pod, what string) *podNotStartedError {
	e := &podNotStartedError{msg: fmt.Sprintf("probe pod %s %s%s", pod.Name, what, waitingReason(pod))}
	if ev := p.newestWarning(pod); ev != nil {
		e.reason, e.eventMessage = ev.Reason, ev.Message
		e.msg += fmt.Sprintf(" (event %s: %s)", ev.Reason, truncate(ev.Message, 200))
	}
	return e
}

// containerRan says whether the probe container started at all (it has or had a running or terminated state).
func containerRan(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return true
		}
	}
	return false
}

// namesDevice says whether a FailedMount message is about one of SubstrateDevices (the kubelet's hostPath
// type check names the path), not about another volume an admission webhook may have added.
func namesDevice(msg string) bool {
	for _, dev := range SubstrateDevices {
		if strings.Contains(msg, dev) {
			return true
		}
	}
	return false
}

// newestWarning is the newest warning event of the pod (by name, and by UID when both carry one), by the
// time it last occurred; nil when there is none or the events cannot be read.
func (p KubeSubstrateProber) newestWarning(pod *corev1.Pod) *corev1.Event {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evs, err := p.Client.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var newest *corev1.Event
	for i := range evs.Items {
		e := &evs.Items[i]
		if e.InvolvedObject.Name != pod.Name || e.Type != corev1.EventTypeWarning {
			continue
		}
		if pod.UID != "" && e.InvolvedObject.UID != "" && e.InvolvedObject.UID != pod.UID {
			continue
		}
		if newest == nil || !eventTime(e).Before(eventTime(newest)) {
			newest = e
		}
	}
	return newest
}

// eventTime is when an event last occurred: its series' last observation, its LastTimestamp, its EventTime or
// its FirstTimestamp, the first one set.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

func (p KubeSubstrateProber) podLogs(ctx context.Context, namespace, name string) (string, error) {
	rc, err := p.Client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Container: "probe"}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	return string(b), err
}

// issuerTLS is the TLS config of the probe. Verified, it trusts what Substrate's api server trusts: the pod's
// serviceaccount CA alone for the in-cluster issuer (the CA file replaces the roots, NewHTTPClient in
// internal/oidcjwt/http.go), otherwise the system roots. Unverified (accepting any certificate) it only tells
// "unreachable" from "not trusted": the answer then yields a failure, never a pass.
func issuerTLS(files IssuerProbeFiles, verify bool) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if !verify {
		cfg.InsecureSkipVerify = true
		return cfg
	}
	if files.InCluster {
		pool := x509.NewCertPool()
		if pem, err := os.ReadFile(files.CAFile); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
		cfg.RootCAs = pool
	}
	return cfg
}

// IsInClusterIssuer reports whether issuer is one for which ate-setup points ate-api-server at the pod's own
// CA and token (an exact match, as its switch).
func IsInClusterIssuer(issuer string) bool { return slices.Contains(inClusterIssuers, issuer) }
