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
)

var (
	// image_pull_secrets = ["a", "b"] under [openshell.drivers.kubernetes]. The keys are unique in the
	// file, so no TOML parser is needed (and none is a dependency). Keys are anchored to a line start.
	pullSecretsKey = regexp.MustCompile(`(?m)^\s*image_pull_secrets\s*=\s*\[([^\]]*)\]`)
	supervisorKey  = regexp.MustCompile(`(?m)^\s*supervisor_image\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	quotedString   = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)
)

const (
	setOnOpenShell = "set server.sandboxImagePullSecrets: [{name: <Secret>}] on the OpenShell release, with the docker-registry Secret in its namespace"
	seeGuide       = "; see the guide \"Private registries and mirrors\""
)

// gatewayImages is what OpenShell's gateway configuration says about the images of sandbox pods.
type gatewayImages struct {
	pullSecrets     []string
	supervisorImage string
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
	return g
}

// checkSandboxImages checks OpenShell's gateway configuration for what sandbox pods need to pull
// the images Koinon names:
//   - when Koinon pulls with credentials or through a mirror (ExpectSandboxPullSecrets), that the
//     gateway has image_pull_secrets (OpenShell's server.sandboxImagePullSecrets), because Koinon
//     cannot give sandbox pods a pull secret itself (ADR 0061);
//   - that the supervisor image the gateway starts is one sandboxPolicy.supervisorImages admits.
//
// It reads one ConfigMap and only warns: an environment variable on the gateway can override the
// file, and a ConfigMap that cannot be read says nothing about the gateway.
func (c *Checker) checkSandboxImages(ctx context.Context) []Result {
	wantSecrets := c.Config.ExpectSandboxPullSecrets
	wantSupervisor := len(c.Config.SupervisorImages) > 0
	if (!wantSecrets && !wantSupervisor) || c.Config.OpenShellConfigMap == "" {
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
		return []Result{{
			Check: checkGatewayConfig, Status: Warn,
			Message: cause + ", so the gateway's sandbox pull secrets and supervisor image are not checked",
			Fix: "set preflight.openshell.namespace and .configMap to the OpenShell release's gateway ConfigMap " +
				"(<fullname>-config), or check by hand that the OpenShell release has server.sandboxImagePullSecrets and " +
				"a supervisor image that sandboxPolicy.supervisorImages lists" + seeGuide,
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
	return results
}
