// SPDX-License-Identifier: Apache-2.0

package templates

// Stage names where a rule is checked. A rule is checked in one place; a fixture proves it.
type Stage string

// The stages, in the order a template meets them.
const (
	// StageParse is the YAML decoding in Parse: one non-empty document.
	StageParse Stage = "parse"
	// StageSchema is schema/template.schema.json, run by Parse.
	StageSchema Stage = "schema"
	// StageCheck is Template.check, run by Parse: the rules the schema cannot express.
	StageCheck Stage = "check"
	// StageLoad is Load: the rules between files.
	StageLoad Stage = "load"
	// StageCatalog is Template.CheckCatalog: names checked against the connector types and tools.
	StageCatalog Stage = "catalog"
	// StageShipped is Template.CheckShipped: the bounds a shipped template sets.
	StageShipped Stage = "shipped"
	// StageBuilt is a Template.check rule that a template file cannot reach while the schema allows
	// one harness. A built value in check_internal_test.go proves it, not a fixture.
	StageBuilt Stage = "built"
)

// Rule is one rule a template must meet. ID is also the stem of its failing fixture:
// testdata/<Dir>/<ID>.yaml, whose "# want:" line is the error text the rule gives.
type Rule struct {
	ID    string
	Stage Stage
	Rule  string
}

// FixtureDir is the directory under testdata that holds the failing fixtures of the stage, and
// false for a stage that has none.
func (s Stage) FixtureDir() (string, bool) {
	switch s {
	case StageParse, StageSchema, StageCheck:
		return "invalid", true
	case StageLoad:
		return "load", true
	case StageCatalog:
		return "catalog", true
	case StageShipped:
		return "shipped", true
	}
	return "", false
}

// Rules is the one table of template rules: what each is and where it is checked. A test fails when
// a rule has no failing fixture, when a fixture has no rule, or when a fixture passes (the rule is
// not enforced). The README lists the same table.
var Rules = []Rule{
	{"empty", StageParse, "The document is not empty."},
	{"two-documents", StageParse, "The file holds exactly one YAML document."},

	{"wrong-api-version", StageSchema, "apiVersion is templates.koinon.dev/v1alpha1."},
	{"wrong-kind", StageSchema, "kind is Template."},
	{"unknown-field", StageSchema, "No field the schema does not name, in spec."},
	{"bad-id", StageSchema, "metadata.id is a lowercase slug."},
	{"bad-version", StageSchema, "metadata.version is SemVer."},
	{"missing-instructions", StageSchema, "spec.instructions is present."},
	{"unknown-harness", StageSchema, "spec.harness names a built-in harness."},
	{"harness-unknown-setting", StageSchema, "spec.harness takes only the settings the schema names."},
	{"harness-not-boolean", StageSchema, "A harness setting is a boolean."},
	{"unknown-preset", StageSchema, "A grant's preset is read-only, standard or trusted."},
	{"bad-scope", StageSchema, "A scope is written as area.action."},
	{"empty-scopes", StageSchema, "A grant lists at least one scope."},
	{"duration-not-a-duration", StageSchema, "limits.run.maxDuration is whole hours, minutes or seconds."},
	{"request-cap-zero", StageSchema, "limits.run.maxModelRequests is at least 1 when set."},
	{"spend-cap-zero", StageSchema, "limits.run.maxSpendUSD is above zero when set."},
	{"spend-as-float", StageSchema, "limits.spendUSD is a string, not a number."},
	{"spend-three-decimals", StageSchema, "limits.spendUSD has at most two fraction digits."},
	{"resources-unknown-field", StageSchema, "spec.resources names only cpu and memory."},
	{"resources-bad-quantity", StageSchema, "A resource value is a Kubernetes quantity."},
	{"questions-zero-duration", StageSchema, "A question timer is above zero."},
	{"notify-user", StageSchema, "A trigger's notify does not name a person."},
	{"notify-channel", StageSchema, "A trigger's notify does not name a chat channel."},
	{"trigger-id-slug", StageSchema, "A trigger ID is a lowercase slug."},
	{"triggers-over-cap", StageSchema, "A template has at most 16 triggers."},
	{"tool-name-bad", StageSchema, "A spec.tools entry is a snake_case tool name."},

	{"duration-over-cap", StageCheck, "limits.run.maxDuration is at most 24h."},
	{"spend-zero", StageCheck, "limits.spendUSD is above zero."},
	{"duplicate-connector", StageCheck, "A template holds one grant per connector."},
	{"targets-min-over-max", StageCheck, "A grant's targets.min is at most targets.max."},
	{"resources-zero", StageCheck, "A resource quantity is above zero."},
	{"resources-request-over-limit", StageCheck, "A resource request is at most its limit."},
	{"questions-reminder-after-expiry", StageCheck, "The question reminder comes before the expiry."},
	{"questions-expiry-over-cap", StageCheck, "A question timer is at most 30 days."},
	{"questions-reminder-under-min", StageCheck, "A question timer is at least one minute."},
	{"trigger-duplicate-id", StageCheck, "Trigger IDs are unique in a template."},
	{"trigger-cron-five-fields", StageCheck, "A schedule's cron has five fields and no timezone prefix."},
	{"trigger-timezone-unknown", StageCheck, "A schedule's timezone is an IANA zone."},
	{"trigger-direct-filter", StageCheck, "A direct_message trigger has no filter."},
	{"trigger-two-direct", StageCheck, "A template has one direct_message trigger."},
	{"trigger-reserved-type", StageCheck, "A trigger type reserved for v0.2 is refused."},
	{"tool-undeclared", StageCheck, "A tool the instructions call in backticks is listed in spec.tools."},

	{"default-harness-not-allowed", StageBuilt, "spec.harness.default is one of spec.harness.allowed."},
	{"plugin-installs-unsupported", StageBuilt, "pluginInstalls needs an allowed harness that supports it."},

	{"file-name-mismatch", StageLoad, "A file is named after its metadata.id."},

	{"unknown-connector", StageCatalog, "A grant names a connector type that exists."},
	{"unknown-scope", StageCatalog, "A grant's scopes exist on its connector type."},
	{"unknown-tool", StageCatalog, "A spec.tools entry is a tool a connector, the platform or a declaration names."},

	{"missing-max-model-requests", StageShipped, "A shipped template sets limits.run.maxModelRequests."},
	{"missing-max-spend", StageShipped, "A shipped template sets limits.run.maxSpendUSD."},
	{"run-spend-over-spend", StageShipped, "limits.run.maxSpendUSD is at most limits.spendUSD."},
	{"missing-targets", StageShipped, "A shipped grant sets targets."},
}
