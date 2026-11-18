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
// in Substrate's namespace (Substrate v1.0.0 docs/get-started/quickstart.md).
func SubstrateRouterSPIFFEID(namespace string) string {
	return "spiffe://cluster.local/ns/" + namespace + "/sa/atenet-client"
}

// SubstrateCRDs are the CRDs the preflight requires (plural.group), verified against the Substrate
// v1.0.0 manifests and the koinon-lab install.
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
	// ExpectedIssuer is the issuer the installer passes to ate-setup (--ateapi-expected-jwt-issuer). Empty
	// means ate-setup reads the issuer from the API server's discovery document, as the probe does.
	ExpectedIssuer string
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
		s.RouterDeployment = "atenet-router"
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
		rctx, cancel := c.callCtx(ctx)
		_, err := c.Issuer.Issuer(rctx)
		cancel()
		if err != nil {
			results = append(results, Result{
				Check: "Service-account issuer", Status: Fail,
				Message: fmt.Sprintf("cannot read the issuer from the API server's discovery document: %v", err),
				Fix:     "grant the preflight ServiceAccount the system:service-account-issuer-discovery ClusterRole",
			})
		}
	}
	return results
}

// SubstrateProbePrerequisites runs the prerequisite checks that start pods in Substrate's namespace: the
// node devices of the vm tier and the service-account issuer probe.
func (c *Checker) SubstrateProbePrerequisites(ctx context.Context) []Result {
	c.defaultSubstrate()
	var results []Result
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
	id := SubstrateRouterSPIFFEID(sub.Namespace)
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		for _, arg := range slices.Concat(ctr.Command, ctr.Args) {
			if v, ok := strings.CutPrefix(arg, SubstrateRouterSPIFFEFlag+"="); ok && slices.Contains(strings.Split(v, ","), id) {
				return Result{Check: check, Status: Pass, Message: "router allows " + id}
			}
		}
	}
	return Result{
		Check: check, Status: Fail,
		Message: fmt.Sprintf("router does not allow %s: actors cannot reach the gateway through atenet (proof P-B)", id),
		Fix:     fmt.Sprintf("start the router with %s=%s (the Koinon installer step sets it)", SubstrateRouterSPIFFEFlag, id),
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
	// Substrate v1.0.0 has no top-level conditions on a WorkerPool: status carries replicas and
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
				Message: fmt.Sprintf("the device probe pod could not run in namespace %s: %v", c.Config.Substrate.Namespace, err),
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

func (c *Checker) checkIssuer(ctx context.Context) Result {
	const check = "Service-account issuer"
	if c.Issuer == nil || c.IssuerProbe == nil {
		return Result{Check: check, Status: Warn, Message: "the issuer probe is disabled; whether Substrate can validate service-account tokens is not checked"}
	}
	rctx, cancel := c.callCtx(ctx)
	discovered, err := c.Issuer.Issuer(rctx)
	cancel()
	if err != nil {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("cannot read the issuer from the API server's discovery document: %v", err),
			Fix:     "grant the preflight ServiceAccount the system:service-account-issuer-discovery ClusterRole",
		}
	}
	// The issuer Substrate's api server trusts is the one ate-setup writes: --ateapi-expected-jwt-issuer when
	// given, else the discovery document's issuer (cmd/ate-setup/internal/steps/create.go jwtIssuer).
	issuer, info := discovered, ""
	if exp := c.Config.Substrate.ExpectedIssuer; exp != "" {
		issuer = exp
		info = fmt.Sprintf(" (the API server's discovery document names %s)", discovered)
	}
	ns := c.Config.Substrate.Namespace
	res, err := c.IssuerProbe.ProbeIssuer(ctx, issuer)
	if err != nil {
		return Result{Check: check, Status: Fail, Message: fmt.Sprintf("issuer probe pod: %v", err)}
	}
	tokenInfo := ""
	if res.TokenIssuer != "" {
		tokenInfo = fmt.Sprintf("; the tokens of pods carry iss %s", res.TokenIssuer)
	}
	if res.Reachable && res.Unverified {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("a pod in %s reaches %s, but only without verifying its TLS certificate (%s); Substrate's api server verifies it and would fail with \"invalid bearer token\"", ns, issuer, res.Detail),
			Fix:     "serve the issuer with a certificate signed by a CA in the system roots of the Substrate image (Substrate trusts only those for an issuer outside the cluster)",
		}
	}
	if !res.Reachable {
		msg := fmt.Sprintf("Substrate's api server could not validate service-account tokens of issuer %s from a pod in namespace %s (%s). "+
			"It fetches <issuer>/.well-known/openid-configuration and the document's jwks_uri, so every call would fail with \"invalid bearer token\"",
			issuer, ns, res.Detail)
		if why := UnroutableIssuerHost(issuer); why != "" {
			msg += ". The issuer host is " + why
		}
		return Result{
			Check: check, Status: Fail, Message: msg + info + tokenInfo,
			Fix: "set the API server's first --service-account-issuer to https://kubernetes.default.svc (Substrate then reads discovery from the in-cluster API server with the pod's token), " +
				"or to a URL that pods reach with a certificate from the system roots (on Talos: cluster.apiServer.extraArgs service-account-issuer). " +
				"Kubernetes signs tokens and names the discovery issuer from that first value, so --expected-jwt-issuer cannot replace it. A sidecar that fakes the issuer is not a supported install",
		}
	}
	if res.TokenIssuer != "" && res.TokenIssuer != issuer {
		return Result{
			Check: check, Status: Fail,
			Message: fmt.Sprintf("Substrate would trust issuer %s%s, but the service-account tokens of pods in %s carry iss %s: its api server rejects every token with \"unexpected issuer\"", issuer, info, ns, res.TokenIssuer),
			Fix: "pass --expected-jwt-issuer " + res.TokenIssuer + " (the value ate-setup would otherwise take from discovery), or make the API server sign tokens with the issuer Substrate trusts " +
				"(the first --service-account-issuer; later ones are only accepted for validation)",
		}
	}
	return Result{Check: check, Status: Pass, Message: fmt.Sprintf("a pod in %s fetches the discovery document and JWKS of %s%s%s", ns, issuer, info, tokenInfo)}
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

// ProbeIssuerFromPod is what the issuer probe pod runs. It repeats what Substrate v1.0.0's api server does
// to validate a service-account token, and nothing else (permalinks at the pinned commit
// b34fbee742906494b5eb4f1f36987176bf8770a4):
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
// So the issuer URL need not be routable when it is the in-cluster one: that is the supported shape on
// clusters whose own issuer is an address pods cannot reach (an Omni or Talos IPv6 address). The probe
// reports the iss claim of its own token so the caller can compare it. The issuer counts as reachable only
// when both fetches answer 2xx. When only an unverified connection works, the result is reachable and
// Unverified. The HTTP status goes into Detail.
func ProbeIssuerFromPod(ctx context.Context, issuer string, timeout time.Duration) IssuerResult {
	return ProbeIssuerWith(ctx, issuer, timeout, IssuerProbeFiles{
		CAFile: IssuerCAFile, TokenFile: IssuerTokenFile,
		InCluster: slices.Contains(inClusterIssuers, strings.TrimSuffix(issuer, "/")),
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
	res := fetchIssuer(ctx, issuer, timeout, files, issuerTLS(files, true))
	if !res.Reachable && (strings.Contains(res.Detail, "certificate") || strings.Contains(res.Detail, "x509")) {
		if unv := fetchIssuer(ctx, issuer, timeout, files, issuerTLS(files, false)); unv.Reachable {
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

func fetchIssuer(ctx context.Context, issuer string, timeout time.Duration, files IssuerProbeFiles, tlsCfg *tls.Config) IssuerResult {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:     (&net.Dialer{Timeout: timeout}).DialContext,
			TLSClientConfig: tlsCfg,
		},
	}
	get := func(target string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		if err != nil {
			return 0, nil, err
		}
		if files.InCluster && issuerScoped(target, issuer) {
			tok, err := os.ReadFile(files.TokenFile)
			if err != nil {
				return 0, nil, fmt.Errorf("read the pod's service-account token: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
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
	if code/100 != 2 {
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
	jcode, _, err := get(doc.JWKSURI)
	if err != nil {
		return IssuerResult{Detail: "jwks_uri " + truncate(doc.JWKSURI, 100) + ": " + truncate(err.Error(), 200)}
	}
	if jcode/100 != 2 {
		return IssuerResult{Detail: fmt.Sprintf("jwks_uri %s: HTTP %d", truncate(doc.JWKSURI, 100), jcode)}
	}
	return IssuerResult{Reachable: true, Detail: fmt.Sprintf("discovery HTTP %d, jwks HTTP %d", code, jcode)}
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

// KubeSubstrateProber runs the issuer probe and the device probe as pods in Substrate's namespace
// (a privileged Pod Security namespace; the issuer pod is unprivileged and carries the default ServiceAccount token to probe with it).
type KubeSubstrateProber struct {
	Client           kubernetes.Interface
	Namespace        string
	NamePrefix       string
	Image            string
	ImagePullSecrets []string
	Labels           map[string]string
	// NodeSelector places the device probe on the worker nodes. The issuer probe does not use it: it
	// checks the network path of the cluster, not of the workers.
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
	if errors.As(err, &notStarted) && notStarted.reason == "FailedMount" {
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
			reason, event := p.warningEvent(name)
			return "", &podNotStartedError{reason: reason, msg: fmt.Sprintf("probe pod %s still %s after %s%s%s", name, pod.Status.Phase, p.timeout(), waitingReason(pod), event)}
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

// podNotStartedError is a probe pod that did not finish in time; reason is the Reason of its newest warning event.
type podNotStartedError struct{ reason, msg string }

func (e *podNotStartedError) Error() string { return e.msg }

// warningEvent is the Reason and the " (event ...)" text of the newest warning event of the pod, for the timeout message.
func (p KubeSubstrateProber) warningEvent(pod string) (reason, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evs, err := p.Client.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", ""
	}
	var msg string
	for _, e := range evs.Items {
		if e.InvolvedObject.Name == pod && e.Type == corev1.EventTypeWarning {
			reason = e.Reason
			msg = fmt.Sprintf("%s: %s", e.Reason, truncate(e.Message, 200))
		}
	}
	if msg == "" {
		return "", ""
	}
	return reason, " (event " + msg + ")"
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
