// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatewayConfigKey is the key of OpenShell's gateway configuration (TOML) in its ConfigMap
// "<fullname>-config" (OpenShell chart 0.1.7, templates/gateway-config.yaml).
const gatewayConfigKey = "gateway.toml"

const (
	checkSandboxPulls    = "Sandbox image pull secrets"
	checkSupervisorImage = "Sandbox supervisor image"
	checkGatewayConfig   = "OpenShell gateway configuration"
	checkGatewayID       = "OpenShell gateway ID"
	checkWorkspaceMode   = "OpenShell workspace mode"
	checkSandboxObserver = "Sandbox observer"
	checkSATokenTTL      = "Sandbox supervisor token lifetime"
)

var (
	// image_pull_secrets = ["a", "b"] under [openshell.drivers.kubernetes]. The keys are unique in the
	// file, so no TOML parser is needed (and none is a dependency). Keys are anchored to a line start.
	pullSecretsKey = regexp.MustCompile(`(?m)^\s*image_pull_secrets\s*=\s*\[([^\]]*)\]`)
	supervisorKey  = regexp.MustCompile(`(?m)^\s*supervisor_image\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	quotedString   = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)
	// A table header, "[openshell.drivers.kubernetes]" (not an array of tables, "[[...]]").
	tableHeader = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*$`)
	stringKey   = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')\s*$`)
	intKey      = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=\s*([0-9][0-9_]*)\s*$`)
)

// kubernetesDriverTable is the table of OpenShell's gateway.toml that holds the Kubernetes driver's
// settings. gateway_id also appears under [openshell.gateway.gateway_jwt], so it is read by section.
const kubernetesDriverTable = "openshell.drivers.kubernetes"

const (
	setOnOpenShell = "set server.sandboxImagePullSecrets: [{name: <Secret>}] on the OpenShell release, with the docker-registry Secret in its namespace"
	seeGuide       = "; see the guide \"Private registries and mirrors\""
)

// gatewayImages is what OpenShell's gateway configuration says about the images of sandbox pods.
type gatewayImages struct {
	pullSecrets     []string
	supervisorImage string
	// gatewayID and workspaceMode are the keys of [openshell.drivers.kubernetes]; empty when unset.
	gatewayID     string
	workspaceMode string
	// saTokenTTL is sa_token_ttl_secs of [openshell.drivers.kubernetes]; 0 when unset.
	saTokenTTL int
}

// stripComment cuts a line at the first # that is outside a basic ("...") or literal ('...') string.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return line[:i]
		}
	}
	return line
}

// parseGatewayConfig reads the two settings out of the gateway.toml text. Comments are ignored,
// whole-line or trailing, and so are # inside strings.
func parseGatewayConfig(toml string) gatewayImages {
	var kept []string
	for line := range strings.SplitSeq(toml, "\n") {
		kept = append(kept, stripComment(line))
	}
	text := strings.Join(kept, "\n")
	var g gatewayImages
	if m := pullSecretsKey.FindStringSubmatch(text); m != nil {
		for _, q := range quotedString.FindAllStringSubmatch(m[1], -1) {
			if name := q[1] + q[2]; name != "" {
				g.pullSecrets = append(g.pullSecrets, name)
			}
		}
	}
	if m := supervisorKey.FindStringSubmatch(text); m != nil {
		g.supervisorImage = m[1] + m[2]
	}
	table := ""
	for line := range strings.SplitSeq(text, "\n") {
		if m := tableHeader.FindStringSubmatch(line); m != nil {
			table = strings.TrimSpace(m[1])
			continue
		}
		if table != kubernetesDriverTable {
			continue
		}
		if m := intKey.FindStringSubmatch(line); m != nil && m[1] == "sa_token_ttl_secs" {
			if n, err := strconv.Atoi(strings.ReplaceAll(m[2], "_", "")); err == nil {
				g.saTokenTTL = n
			}
			continue
		}
		if m := stringKey.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "gateway_id":
				g.gatewayID = m[2] + m[3]
			case "workspace_mode":
				g.workspaceMode = m[2] + m[3]
			}
		}
	}
	return g
}

// GatewayConfigKey is the key of OpenShell's gateway configuration in its ConfigMap.
const GatewayConfigKey = gatewayConfigKey

// SupervisorImage reads the gateway's supervisor_image out of the gateway.toml text; empty when the
// file does not set it. The reconciler uses it for the sandbox spec hash (ADR 0085 decision 9).
func SupervisorImage(toml string) string {
	return parseGatewayConfig(toml).supervisorImage
}

// GatewayID reads the gateway's gateway_id out of the gateway.toml text, as the gateway uses it: the
// key under [openshell.drivers.kubernetes], or OpenShell's default when the file does not set one.
// The reconciler uses it to notice that the admission policies no longer match the gateway's
// namespaces (old issue 475).
func GatewayID(toml string) string {
	if id := parseGatewayConfig(toml).gatewayID; id != "" {
		return id
	}
	return openShellDefaultGatewayID
}

// checkSandboxImages checks OpenShell's gateway configuration for what sandbox pods need to pull
// the images Koinon names:
//   - when Koinon pulls with credentials or through a mirror (ExpectSandboxPullSecrets), that the
//     gateway has image_pull_secrets (OpenShell's server.sandboxImagePullSecrets), because Koinon
//     cannot give sandbox pods a pull secret itself (ADR 0061);
//   - that the supervisor image the gateway starts is one sandboxPolicy.supervisorImages admits.
//
// It reads one ConfigMap. Its findings only warn: an environment variable on the gateway can override
// the file. A ConfigMap that cannot be read fails when the chart configures a supervisor image
// (sandboxPolicy.supervisorImages): the reconciler reads the same ConfigMap to put the supervisor image
// in the sandbox spec hash (ADR 0085, old issue 410), so it would not recreate sandboxes after an OpenShell
// upgrade (old issue 489). Without a supervisor image it warns, since it says nothing about the gateway.
func (c *Checker) checkSandboxImages(ctx context.Context) []Result {
	wantSecrets := c.Config.ExpectSandboxPullSecrets
	wantSupervisor := len(c.Config.SupervisorImages) > 0
	wantGatewayID := c.Config.OpenShellGatewayID != ""
	wantTTL := c.Config.SupervisorTokenMaxSeconds > 0
	if (!wantSecrets && !wantSupervisor && !wantGatewayID && !wantTTL) || c.Config.OpenShellConfigMap == "" {
		return nil
	}
	ns, name := c.Config.OpenShellConfigNamespace, c.Config.OpenShellConfigMap
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	cm, err := c.Kube.CoreV1().ConfigMaps(ns).Get(cctx, name, metav1.GetOptions{})
	var cause string
	switch {
	case err != nil:
		cause = fmt.Sprintf("ConfigMap %s/%s could not be read: %v", ns, name, err)
	case cm.Data[gatewayConfigKey] == "":
		cause = fmt.Sprintf("ConfigMap %s/%s has no %s", ns, name, gatewayConfigKey)
	}
	if cause != "" {
		status := Warn
		if wantSupervisor {
			status = Fail
		}
		return []Result{{
			Check: checkGatewayConfig, Status: status,
			Message: cause + ", so the gateway's sandbox pull secrets, supervisor image and gateway ID are not checked, " +
				"and the reconciler cannot see an OpenShell upgrade: sandboxes are not recreated for a new supervisor image",
			Fix: "set preflight.openshell.namespace and .configMap to the OpenShell release's gateway ConfigMap " +
				"(<fullname>-config), or check by hand that the OpenShell release has server.sandboxImagePullSecrets and " +
				"a supervisor image that sandboxPolicy.supervisorImages lists, and a gateway_id that equals openshell.gatewayId" + seeGuide,
		}}
	}
	g := parseGatewayConfig(cm.Data[gatewayConfigKey])

	var results []Result
	if wantSecrets {
		r := Result{Check: checkSandboxPulls}
		if len(g.pullSecrets) == 0 {
			r.Status = Warn
			r.Message = "the OpenShell gateway has no image_pull_secrets, but this release pulls with credentials or " +
				"through a mirror: sandbox pods (workload and supervisor) will not get a pull secret and may stay in " +
				"ImagePullBackOff. Ignore this if the registry or mirror needs no credentials, or the nodes hold them. " +
				"A gateway environment variable can override the file this reads"
			r.Fix = setOnOpenShell + seeGuide
		} else {
			r.Status = Pass
			r.Message = fmt.Sprintf("the OpenShell gateway gives sandbox pods the pull secrets %s", strings.Join(g.pullSecrets, ", "))
		}
		results = append(results, r)
	}
	if wantSupervisor {
		r := Result{Check: checkSupervisorImage}
		switch {
		case g.supervisorImage == "":
			r.Status = Warn
			r.Message = "the OpenShell gateway configuration does not set supervisor_image, so its supervisor image is unknown"
			r.Fix = "check by hand that sandboxPolicy.supervisorImages (or its default) lists the image the gateway starts"
		case !slices.Contains(c.Config.SupervisorImages, g.supervisorImage):
			r.Status = Warn
			r.Message = fmt.Sprintf("the OpenShell gateway starts supervisor pods with %s, which sandboxPolicy denies "+
				"(it admits %s): every sandbox would fail", g.supervisorImage, strings.Join(c.Config.SupervisorImages, ", "))
			r.Fix = "add " + g.supervisorImage + " to sandboxPolicy.supervisorImages, or make global.imageRegistry / " +
				"global.registryMirrors rewrite the default to it (and leave sandboxPolicy.supervisorImages empty)"
		default:
			r.Status = Pass
			r.Message = fmt.Sprintf("the gateway's supervisor image %s is admitted by sandboxPolicy", g.supervisorImage)
		}
		results = append(results, r)
	}
	if wantTTL {
		results = append(results, c.tokenTTLResult(ctx, g))
	}
	if wantGatewayID {
		results = append(results, c.gatewayIDResults(g)...)
	}
	return results
}

// OpenShell's supervisor token lifetime (driver-kubernetes config.rs; ADR 0006): default 3600 seconds,
// clamped to 600-86400.
const (
	openShellDefaultSATokenTTL = 3600
	openShellMinSATokenTTL     = 600
	openShellMaxSATokenTTL     = 86400
)

// tokenTTLEnv is the name of the gateway environment variable that overrides sa_token_ttl_secs.
const tokenTTLEnv = "OPENSHELL_K8S_SA_TOKEN_TTL_SECS"

// gatewayTokenTTLEnv reads OPENSHELL_K8S_SA_TOKEN_TTL_SECS from the gateway workload (a StatefulSet, or
// a Deployment) in the OpenShell namespace. It returns the value (0 when the variable is not set or
// is not a number, which OpenShell ignores in favour of the file) and ok=false when the workload could
// not be read, a container has envFrom entries (the variable may come from them), or the variable comes from a Secret, ConfigMap or field reference, so its value is unknown.
func (c *Checker) gatewayTokenTTLEnv(ctx context.Context) (value int, ok bool) {
	ns, name := c.Config.OpenShellConfigNamespace, c.Config.OpenShellWorkload
	if name == "" {
		return 0, false
	}
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	var containers []corev1.Container
	if sts, err := c.Kube.AppsV1().StatefulSets(ns).Get(cctx, name, metav1.GetOptions{}); err == nil {
		containers = sts.Spec.Template.Spec.Containers
	} else if dep, derr := c.Kube.AppsV1().Deployments(ns).Get(cctx, name, metav1.GetOptions{}); derr == nil {
		containers = dep.Spec.Template.Spec.Containers
	} else {
		return 0, false
	}
	for _, ct := range containers {
		for _, e := range ct.Env {
			if e.Name != tokenTTLEnv {
				continue
			}
			if e.ValueFrom != nil {
				return 0, false
			}
			if n, err := strconv.Atoi(strings.TrimSpace(e.Value)); err == nil {
				return n, true
			}
			return 0, true
		}
		// envFrom (ConfigMap or Secret) may carry the variable; its value is unknown.
		if len(ct.EnvFrom) > 0 {
			return 0, false
		}
	}
	return 0, true
}

// tokenTTLResult compares the lifetime OpenShell gives the supervisor's ServiceAccount token with
// sandboxPolicy.supervisorTokenMaxSeconds. The policy denies a supervisor pod whose expirationSeconds is
// above the bound, so a longer TTL means no sandbox starts. The gateway's OPENSHELL_K8S_SA_TOKEN_TTL_SECS
// wins over sa_token_ttl_secs in its configuration file, so the check reads both. When the workload
// cannot be read, the environment variable is unknown and the result is a warning, never a pass.
func (c *Checker) tokenTTLResult(ctx context.Context, g gatewayImages) Result {
	ttl, bound := g.saTokenTTL, c.Config.SupervisorTokenMaxSeconds
	envTTL, observed := c.gatewayTokenTTLEnv(ctx)
	if envTTL > 0 {
		ttl = envTTL
	}
	if ttl == 0 {
		ttl = openShellDefaultSATokenTTL
	}
	ttl = min(max(ttl, openShellMinSATokenTTL), openShellMaxSATokenTTL)
	r := Result{Check: checkSATokenTTL}
	switch {
	case !observed:
		r.Status = Warn
		r.Message = fmt.Sprintf("the gateway workload %s/%s could not be read (or sets %s from a reference or envFrom), so %s was not checked; "+
			"from its configuration file the supervisor token lifetime is %d seconds against sandboxPolicy.supervisorTokenMaxSeconds=%d. "+
			"If the environment variable is above the bound, the sandbox admission policy denies every supervisor pod",
			c.Config.OpenShellConfigNamespace, c.Config.OpenShellWorkload, tokenTTLEnv, tokenTTLEnv, ttl, bound)
		r.Fix = fmt.Sprintf("check by hand that %s on the OpenShell gateway is at most %d (or unset), or set preflight.openshell.workload to the gateway's StatefulSet", tokenTTLEnv, bound)
	case ttl > bound:
		r.Status = Fail
		r.Message = fmt.Sprintf("the OpenShell gateway gives supervisor ServiceAccount tokens a lifetime of %d seconds, above "+
			"sandboxPolicy.supervisorTokenMaxSeconds=%d: the sandbox admission policy denies every supervisor pod, so no sandbox starts", ttl, bound)
		r.Fix = fmt.Sprintf("set sandboxPolicy.supervisorTokenMaxSeconds to at least %d, or lower %s (sa_token_ttl_secs) on the OpenShell gateway to at most %d", ttl, tokenTTLEnv, bound)
	default:
		r.Status = Pass
		r.Message = fmt.Sprintf("the gateway's supervisor token lifetime (%d seconds, from its configuration and environment) is within sandboxPolicy.supervisorTokenMaxSeconds (%d)", ttl, bound)
	}
	return r
}

// openShellDefaultGatewayID is the gateway_id OpenShell uses when gateway.toml sets none: the
// default of its server config (NVIDIA/OpenShell crates/openshell-server/src/defaults.rs,
// gateway_id: "openshell") and of the Kubernetes driver's OPENSHELL_GATEWAY_ID
// (DEFAULT_GATEWAY_ID); ADR 0078 decision 3 records the same default.
const openShellDefaultGatewayID = "openshell"

// gatewayIDResults compares openshell.gatewayId with the gateway's gateway_id and checks the workspace
// mode: the sandbox observer finds a workspace's namespace as openshell-<gateway_id>-<workspace>, and
// the chart's policy admits its RoleBinding only in namespaces labelled with that gateway ID (ADR 0078).
func (c *Checker) gatewayIDResults(g gatewayImages) []Result {
	want := c.Config.OpenShellGatewayID
	r := Result{Check: checkGatewayID}
	switch {
	case g.gatewayID == "":
		r.Status = Warn
		r.Message = fmt.Sprintf("the OpenShell gateway configuration does not set gateway_id under [%s], so it is not compared with openshell.gatewayId=%s", kubernetesDriverTable, want)
		r.Fix = "check by hand that the OpenShell release's gatewayId equals openshell.gatewayId"
		if c.Config.SandboxPolicyScopedToGateway {
			// The gateway falls back to its default gateway_id, so an unset key is a match only when
			// openshell.gatewayId is that default. Otherwise the policies bind to namespaces that carry
			// another ID and cover none of this gateway's sandboxes.
			if want == openShellDefaultGatewayID {
				r.Message += fmt.Sprintf("; openshell.gatewayId is OpenShell's default gateway_id (%s), which the gateway uses when the key is unset", openShellDefaultGatewayID)
			} else {
				r.Status = Fail
				r.Message = fmt.Sprintf("the OpenShell gateway configuration does not set gateway_id under [%s], so the gateway uses OpenShell's default (%q), but openshell.gatewayId is %q: "+
					"the sandbox admission policies bind only to namespaces labelled openshell.ai/gateway-id=%s, so they would apply to none of this gateway's sandboxes and the sandbox boundary would fail open",
					kubernetesDriverTable, openShellDefaultGatewayID, want, want)
				r.Fix = "set openshell.gatewayId=" + openShellDefaultGatewayID + " on the Koinon release, or set gatewayId=" + want + " on the OpenShell release"
			}
		}
	case g.gatewayID != want:
		r.Status = Warn
		r.Message = fmt.Sprintf("the OpenShell gateway's gateway_id is %q but openshell.gatewayId is %q: the sandbox observer would look for "+
			"sandbox pods in the wrong namespaces, and the sandbox policy would refuse its RoleBinding there", g.gatewayID, want)
		if c.Config.SandboxPolicyScopedToGateway {
			r.Status = Fail
			r.Message = fmt.Sprintf("the OpenShell gateway's gateway_id is %q but openshell.gatewayId is %q: the sandbox admission policies bind only to "+
				"namespaces labelled openshell.ai/gateway-id=%s, so they would apply to none of this gateway's sandboxes and the sandbox boundary would "+
				"fail open (the sandbox observer would also look in the wrong namespaces)", g.gatewayID, want, want)
		}
		r.Fix = "set openshell.gatewayId=" + g.gatewayID + " on the Koinon release (or the OpenShell release's gatewayId to " + want + ")"
	default:
		r.Status = Pass
		r.Message = fmt.Sprintf("openshell.gatewayId matches the gateway's gateway_id (%s)", want)
	}
	results := []Result{r}
	if g.workspaceMode != "" && g.workspaceMode != "managed" {
		results = append(results, Result{
			Check: checkWorkspaceMode, Status: Warn,
			Message: fmt.Sprintf("the OpenShell gateway's workspace_mode is %q: Koinon expects managed (ADR 0002), where OpenShell creates a namespace per workspace", g.workspaceMode),
			Fix:     "set the OpenShell release's workspaceMode to managed",
		})
	}
	return results
}

// checkSandboxObserver warns when the observer is off: a sandbox that cannot pull its image or keeps
// crashing then shows only OpenShell's reason, so its wait cause is unknown (ADR 0078).
func (c *Checker) checkSandboxObserver() []Result {
	if !c.Config.SandboxObserverDisabled {
		return nil
	}
	return []Result{{
		Check: checkSandboxObserver, Status: Warn,
		Message: "sandboxObserver.enabled is false: when a sandbox cannot pull its image or keeps crashing, OpenShell reports only " +
			"ConfigurationPending, so the run fails with the cause \"unknown\" instead of image_pull or crash_loop",
		Fix: "set sandboxObserver.enabled=true (the default) to let the reconciler read sandbox pods with get; see the guide \"Agent health\"",
	}}
}
