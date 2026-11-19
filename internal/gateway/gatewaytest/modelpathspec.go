// SPDX-License-Identifier: Apache-2.0

package gatewaytest

// RegisterModel adds the model cases of the behaviour suite for a gateway that serves the model path
// alone and does not translate between formats (Env.NoTranslation): the Messages cases and the usage
// contract, judged on the record the spend ledger reads (Gateway.Usage). The MCP, guard, budget and
// revision cases of [Register] are not here: they need the other half, or the budget stop (#279 S2).
func RegisterModel(env Env) {
	anthropicSpec(env)
	usageSpec(env)
}
