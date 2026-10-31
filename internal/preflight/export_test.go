// SPDX-License-Identifier: Apache-2.0

package preflight

var (
	ParseStatus        = parseStatus
	ImageTag           = imageTag
	CheckNetworkPolicy = (*Checker).checkNetworkPolicy
)

var ParseGatewayConfig = func(toml string) (pullSecrets []string, supervisorImage string) {
	g := parseGatewayConfig(toml)
	return g.pullSecrets, g.supervisorImage
}

// ParseGatewayKubernetesDriver returns gateway_id and workspace_mode of [openshell.drivers.kubernetes].
var ParseGatewayKubernetesDriver = func(toml string) (gatewayID, workspaceMode string) {
	g := parseGatewayConfig(toml)
	return g.gatewayID, g.workspaceMode
}
