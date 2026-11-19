// SPDX-License-Identifier: Apache-2.0

package gatewaytest

// RegisterModel adds the model cases of the behaviour suite for a gateway that serves the model path
// alone and does not translate between formats (Env.NoTranslation): the Messages cases and the usage
// contract and the budget stop, judged on the record the spend ledger reads (Gateway.Usage). The MCP,
// guard and revision cases of [Register] are not here: they need the other half.
func RegisterModel(env Env) {
	anthropicSpec(env)
	usageSpec(env)
	budgetOvershootSpec(env)
}
