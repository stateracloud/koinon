// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatewayConfigKey is the key of OpenShell's gateway configuration (TOML) in its ConfigMap
// "<fullname>-config" (OpenShell chart 0.1.5, templates/gateway-config.yaml).
const gatewayConfigKey = "gateway.toml"

const (
	checkSandboxPulls    = "Sandbox image pull secrets"
	checkSupervisorImage = "Sandbox supervisor image"
	checkGatewayConfig   = "OpenShell gateway configuration"
	checkGatewayID       = "OpenShell gateway ID"
	checkWorkspaceMode   = "OpenShell workspace mode"
	checkSandboxObserver = "Sandbox observer"
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
	if (!wantSecrets && !wantSupervisor && !wantGatewayID) || c.Config.OpenShellConfigMap == "" {
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
	if wantGatewayID {
		results = append(results, c.gatewayIDResults(g)...)
	}
	return results
}

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
	case g.gatewayID != want:
		r.Status = Warn
		r.Message = fmt.Sprintf("the OpenShell gateway's gateway_id is %q but openshell.gatewayId is %q: the sandbox observer would look for "+
			"sandbox pods in the wrong namespaces, and the sandbox policy would refuse its RoleBinding there", g.gatewayID, want)
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
