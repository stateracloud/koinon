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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	// SubstrateRouterSPIFFEID is the identity the flag must allow.
	SubstrateRouterSPIFFEID = "spiffe://cluster.local/ns/ate-system/sa/atenet-client"

	// SubstrateIssuerProbeMarker starts the issuer probe's output line.
	SubstrateIssuerProbeMarker = "koinon-issuer-probe"
	// SubstrateDevicesProbeMarker starts the device probe's output line.
	SubstrateDevicesProbeMarker = "koinon-devices-probe"

	substrateInstallDoc = "install the pinned Substrate with the Koinon installer step (koinon-api substrate install)"
)

// substrateVersion matches the supported Substrate releases: v1.x carries the compatibility promise
// of v1.0.0 (ADR 0119); Koinon tests the pinned release, and a later patch of the line passes.
var substrateVersion = regexp.MustCompile(`^v1\.[0-9]+\.[0-9]+$`)

// SubstrateCRDs are the CRDs the preflight requires (plural.group). The first is verified against
// v1.0.0 (ADR 0119, upstream verification); the others are the objects D7 names.
var SubstrateCRDs = []string{
	"sandboxconfigs." + SubstrateCRDGroup,
	"workerpools." + SubstrateCRDGroup,
	"csidriverconfigs." + SubstrateCRDGroup,
}

// SubstrateDevices are the character devices a `vm` tier node needs: KVM, the TUN device for the
// guest network, and the vhost devices (their modules are vhost_vsock and vhost_net).
var SubstrateDevices = []string{"/dev/kvm", "/dev/net/tun", "/dev/vhost-vsock", "/dev/vhost-net"}

// SubstrateConfig is what the Substrate driver expects of the cluster.
type SubstrateConfig struct {
	// Namespace is Substrate's fixed namespace (ate-system).
	Namespace string
	// APIServerDeployment carries the release version in its image tag.
	APIServerDeployment string
	// RouterDeployment runs the router whose SPIFFE allow-list is checked.
	RouterDeployment string
	// WorkerPools maps a tier ("vm", "hardened") to the WorkerPool that must be ready. Empty skips.
	WorkerPools map[string]string
	// NodeSelector limits the device probe to the nodes that run workers.
	NodeSelector map[string]string
	// VMTier is true when the vm tier is in use: the nodes need the microVM devices.
	VMTier bool
}

// WithDefaults fills the fixed names of Substrate v1.0.0.
func (s SubstrateConfig) WithDefaults() SubstrateConfig {
	if s.Namespace == "" {
		s.Namespace = "ate-system"
	}
	if s.APIServerDeployment == "" {
		s.APIServerDeployment = "ate-api-server"
	}
	if s.RouterDeployment == "" {
		s.RouterDeployment = "ate-router"
	}
	return s
}

// IssuerResolver returns the service-account token issuer URL of the cluster.
type IssuerResolver interface {
	Issuer(ctx context.Context) (string, error)
}

// IssuerResult is what an issuer probe pod saw.
type IssuerResult struct {
	Reachable bool
	Detail    string
}

// String formats the result as the probe prints it.
func (r IssuerResult) String() string {
	return fmt.Sprintf("%s reachable=%t detail=%q", SubstrateIssuerProbeMarker, r.Reachable, r.Detail)
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
// devices of the vm tier and the service-account issuer. The installer step runs them before it
// installs anything, so a cluster that cannot run Substrate fails before it changes.
func (c *Checker) SubstratePrerequisites(ctx context.Context) []Result {
	c.defaultSubstrate()
	results := []Result{c.checkKubernetesVersion(ctx)}
	if c.Config.Substrate.VMTier {
		results = append(results, c.checkSubstrateDevices(ctx))
	}
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
	if cs := deploy.Spec.Template.Spec.Containers; len(cs) > 0 {
		image = cs[0].Image
	}
	tag := imageTag(image)
	switch {
	case tag == "":
		return Result{
			Check: check, Status: Warn,
			Message: fmt.Sprintf("api server image %q has no tag, so its version is unknown", image),
			Fix:     "deploy the api server from a tagged release image",
		}
	case !substrateVersion.MatchString(tag):
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("api server version %s is not supported; Koinon supports Substrate v1.x", tag),
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
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		for _, arg := range slices.Concat(ctr.Command, ctr.Args) {
			if v, ok := strings.CutPrefix(arg, SubstrateRouterSPIFFEFlag+"="); ok && slices.Contains(strings.Split(v, ","), SubstrateRouterSPIFFEID) {
				return Result{Check: check, Status: Pass, Message: "router allows " + SubstrateRouterSPIFFEID}
			}
		}
	}
	return Result{
		Check: check, Status: Fail,
		Message: fmt.Sprintf("router does not allow %s: actors cannot reach the gateway through atenet (proof P-B)", SubstrateRouterSPIFFEID),
		Fix:     fmt.Sprintf("start the router with %s=%s (the Koinon installer step sets it)", SubstrateRouterSPIFFEFlag, SubstrateRouterSPIFFEID),
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
		results = append(results, c.checkWorkerPool(ctx, check, sub.Namespace, sub.WorkerPools[tier], crd))
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
			Fix:     "create it with the installer step, or set the pool name of this tier in the chart values",
		}
	case err != nil:
		return apiFailure(check, fmt.Sprintf("WorkerPool %s/%s", namespace, name), err)
	}
	conds, _, _ := unstructured.NestedSlice(pool.Object, "status", "conditions")
	for _, cond := range conds {
		if m, ok := cond.(map[string]any); ok && m["type"] == "Ready" {
			if m["status"] == "True" {
				return Result{Check: check, Status: Pass, Message: fmt.Sprintf("WorkerPool %s/%s ready", namespace, name)}
			}
			return Result{
				Check: check, Status: Fail,
				Message: fmt.Sprintf("WorkerPool %s/%s is not ready: %v", namespace, name, m["message"]),
				Fix:     "check the worker pods and the atelet logs in namespace " + namespace,
			}
		}
	}
	return Result{
		Check: check, Status: Fail,
		Message: fmt.Sprintf("WorkerPool %s/%s reports no Ready condition", namespace, name),
		Fix:     "wait for Substrate's controller to reconcile it, then check the controller logs in namespace " + namespace,
	}
}

func (c *Checker) checkSubstrateDevices(ctx context.Context) Result {
	const check = "Substrate node devices"
	if c.Devices == nil {
		return Result{Check: check, Status: Warn, Message: "the device probe is disabled; /dev/kvm and the other microVM devices are not checked"}
	}
	if err := c.Devices.ProbeDevices(ctx); err != nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("a node lacks one of %s: %v", strings.Join(SubstrateDevices, ", "), err),
			Fix: "run the vm tier on nodes with KVM (nested virtualisation on cloud VMs), load the modules vhost_vsock and vhost_net, " +
				"and tun; or use the hardened tier (gVisor), which needs no KVM",
		}
	}
	return Result{Check: check, Status: Pass, Message: "one node mounted " + strings.Join(SubstrateDevices, ", ") + " (only that node is probed)"}
}

func (c *Checker) checkIssuer(ctx context.Context) Result {
	const check = "Service-account issuer"
	if c.Issuer == nil || c.IssuerProbe == nil {
		return Result{Check: check, Status: Warn, Message: "the issuer probe is disabled; whether Substrate can fetch the OIDC discovery document is not checked"}
	}
	rctx, cancel := c.callCtx(ctx)
	issuer, err := c.Issuer.Issuer(rctx)
	cancel()
	if err != nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("cannot read the issuer from the API server's discovery document: %v", err),
			Fix:     "grant the preflight ServiceAccount the system:service-account-issuer-discovery ClusterRole",
		}
	}
	res, err := c.IssuerProbe.ProbeIssuer(ctx, issuer)
	if err != nil {
		return Result{Check: check, Status: Fail, Message: fmt.Sprintf("issuer probe pod: %v", err)}
	}
	if res.Reachable {
		return Result{Check: check, Status: Pass, Message: fmt.Sprintf("a pod in %s reaches %s", c.Config.Substrate.Namespace, issuer)}
	}
	msg := fmt.Sprintf("the issuer %s is not reachable from a pod in namespace %s (%s). "+
		"Substrate's api server fetches the OIDC discovery document and the JWKS from this URL, so every call would fail with \"invalid bearer token\"",
		issuer, c.Config.Substrate.Namespace, res.Detail)
	if why := UnroutableIssuerHost(issuer); why != "" {
		msg += ". The issuer host is " + why
	}
	return Result{
		Check: check, Status: Fail, Message: msg,
		Fix: "make the issuer routable from pods: set the API server's --service-account-issuer (and --service-account-jwks-uri) to a URL that pods reach " +
			"(on Talos: cluster.apiServer.extraArgs service-account-issuer), or wait for an upstream Substrate discovery-URL override. " +
			"A sidecar that fakes the issuer is not a supported install",
	}
}

// issuerDiscoveryPath is the discovery path the probe fetches under the issuer.
const issuerDiscoveryPath = "/.well-known/openid-configuration"

// ProbeIssuerFromPod is what the issuer probe pod runs: it dials the issuer and asks for the
// discovery document. It sends no credential and does not verify the certificate, because it checks
// that the URL is reachable, not that it is trusted; any HTTP answer counts.
func ProbeIssuerFromPod(ctx context.Context, issuer string, timeout time.Duration) IssuerResult {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return IssuerResult{Detail: "not a URL"}
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
			// Reachability only: no credential is sent and the answer is not trusted.
			TLSClientConfig: reachabilityTLS(),
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(issuer, "/")+issuerDiscoveryPath, http.NoBody)
	if err != nil {
		return IssuerResult{Detail: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return IssuerResult{Detail: truncate(err.Error(), 200)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return IssuerResult{Reachable: true, Detail: "HTTP " + resp.Status}
}

// ParseIssuerProbeOutput reads the probe line from the probe pod's log.
func ParseIssuerProbeOutput(log string) (IssuerResult, error) {
	for line := range strings.Lines(log) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), SubstrateIssuerProbeMarker+" ")
		if !ok {
			continue
		}
		var res IssuerResult
		reach, detail, _ := strings.Cut(rest, " detail=")
		switch reach {
		case "reachable=true":
			res.Reachable = true
		case "reachable=false":
		default:
			return IssuerResult{}, fmt.Errorf("malformed probe output %q", truncate(line, 200))
		}
		res.Detail, _ = strconv.Unquote(detail)
		return res, nil
	}
	return IssuerResult{}, fmt.Errorf("no %q line in the probe pod's log: %q", SubstrateIssuerProbeMarker, truncate(log, 200))
}

// KubeSubstrateProber runs the issuer probe and the device probe as pods in Substrate's namespace
// (a privileged Pod Security namespace; the issuer pod is unprivileged and needs no token).
type KubeSubstrateProber struct {
	Client           kubernetes.Interface
	Namespace        string
	NamePrefix       string
	Image            string
	ImagePullSecrets []string
	Labels           map[string]string
	NodeSelector     map[string]string
	Timeout          time.Duration
	PollInterval     time.Duration
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
			NodeSelector:                 p.NodeSelector,
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

// IssuerPod is the issuer probe pod.
func (p KubeSubstrateProber) IssuerPod(issuer string) *corev1.Pod {
	return p.pod("issuer-probe", []string{"preflight", "issuer-probe", "--issuer", issuer}, nil, nil)
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
	return p.pod("devices-probe", []string{"preflight", "devices-probe"}, volumes, mounts)
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
			return "", fmt.Errorf("probe pod %s still %s after %s%s%s", name, pod.Status.Phase, p.timeout(), waitingReason(pod), p.eventReason(name))
		case <-time.After(interval):
		}
		if pod, err = pods.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return "", fmt.Errorf("read probe pod %s: %w", name, err)
		}
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

// eventReason is the newest warning event of the pod, for the timeout message.
func (p KubeSubstrateProber) eventReason(pod string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evs, err := p.Client.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	var msg string
	for _, e := range evs.Items {
		if e.InvolvedObject.Name == pod && e.Type == corev1.EventTypeWarning {
			msg = fmt.Sprintf("%s: %s", e.Reason, truncate(e.Message, 200))
		}
	}
	if msg == "" {
		return ""
	}
	return " (event " + msg + ")"
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

// reachabilityTLS is a TLS config that accepts any certificate: the probe checks that the URL
// answers, sends no credential and does not trust the answer.
func reachabilityTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // reachability only
}
