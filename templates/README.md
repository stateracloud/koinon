# templates

Agent templates: building blocks for the agent builder; the result is always a draft (ADR 0020).

- Templates: `repo-helper` and `repo-helper-gitea` (small pull requests), `finops-analyst` (daily cost
  report from MCP cost servers, no grant), `researcher` (private-only research over repositories, several runs, no web reads),
  `web-researcher` (web research over several runs, no grant, no private data) and
  `lcm-upgrader` (weekly upgrade proposals, one pull request after a yes).
- `researcher` and `web-researcher` are split so that no template holds all three legs of ADR 0084
  (untrusted input, private data, an auto-tier `external-high` tool; fetch and search are
  `external-low`, ADR 0091). `legs_test.go` fails if a template combines them, with no exceptions;
  `testdata/legs/` has the negative fixtures. The test takes the legs only from the tools in
  `spec.tools`, classified with the declarations in `testdata/legs/tool-declarations.json`. `Parse` fails when the
  instructions name a tool in backticks that `spec.tools` does not list. To add
  a tool to a template, add it to `spec.tools` and, if it is not an unclassified default tool, to
  that file.
- `<id>.yaml`: one template per file, named after `metadata.id`, `apiVersion:
  templates.koinon.dev/v1alpha1`, `kind: Template`.
- `schema/template.schema.json`: the JSON Schema (2020-12) every template must pass.
- `templates.go`: embeds the templates into the binary, validates them (schema plus the rules a
  schema cannot express, such as the default harness being allowed) and computes a content hash over
  the canonical JSON form, so comments and formatting do not change it.
- `testdata/`: a minimal valid template and invalid fixtures; each invalid file names the error it
  must produce on a `# want:` line.

## Rules

One table, `Rules` in `rules.go`, names every rule and where it is checked; the table below lists the
same rows (a test fails when the two differ). Each rule has a failing fixture, named after its ID, with
a `# want:` line that gives the error text. `rules_test.go` runs each fixture at its stage and fails
when the fixture passes, so a rule that is removed fails its own fixture. The stages:

- `Parse` decodes the YAML, then runs the schema, then `Template.check`, the rules the schema cannot
  express. The schema's errors start with `schema:`; the test holds each rule to its stage.
- `Load` checks the rules between files.
- `CheckCatalog` checks the names a template uses against the connector types and the known tools.
  `templates` cannot import `connectors` (the import goes the other way), so the caller passes a
  `Catalog`. `CheckEmbedded(Catalog)` runs `CheckShipped` and `CheckCatalog` over every embedded
  template; `rules_test.go` runs it with the catalog of the `connectors` package.
- `CheckShipped` holds a template that ships in the binary to bounds a user-written template may
  leave to the platform defaults: both run bounds are set, the run spend is within the spend limit,
  and every grant has targets.

| Rule | Checked in | Rule text | Failing fixture |
|---|---|---|---|
| `empty` | `Parse` (YAML) | The document is not empty. | `testdata/invalid/empty.yaml` |
| `two-documents` | `Parse` (YAML) | The file holds exactly one YAML document. | `testdata/invalid/two-documents.yaml` |
| `wrong-api-version` | `schema/template.schema.json` | apiVersion is templates.koinon.dev/v1alpha1. | `testdata/invalid/wrong-api-version.yaml` |
| `wrong-kind` | `schema/template.schema.json` | kind is Template. | `testdata/invalid/wrong-kind.yaml` |
| `unknown-field` | `schema/template.schema.json` | No field the schema does not name, in spec. | `testdata/invalid/unknown-field.yaml` |
| `bad-id` | `schema/template.schema.json` | metadata.id is a lowercase slug. | `testdata/invalid/bad-id.yaml` |
| `bad-version` | `schema/template.schema.json` | metadata.version is SemVer. | `testdata/invalid/bad-version.yaml` |
| `missing-instructions` | `schema/template.schema.json` | spec.instructions is present. | `testdata/invalid/missing-instructions.yaml` |
| `unknown-harness` | `schema/template.schema.json` | spec.harness names a built-in harness. | `testdata/invalid/unknown-harness.yaml` |
| `harness-unknown-setting` | `schema/template.schema.json` | spec.harness takes only the settings the schema names. | `testdata/invalid/harness-unknown-setting.yaml` |
| `harness-not-boolean` | `schema/template.schema.json` | A harness setting is a boolean. | `testdata/invalid/harness-not-boolean.yaml` |
| `unknown-preset` | `schema/template.schema.json` | A grant's preset is read-only, standard or trusted. | `testdata/invalid/unknown-preset.yaml` |
| `bad-scope` | `schema/template.schema.json` | A scope is written as area.action. | `testdata/invalid/bad-scope.yaml` |
| `empty-scopes` | `schema/template.schema.json` | A grant lists at least one scope. | `testdata/invalid/empty-scopes.yaml` |
| `duration-not-a-duration` | `schema/template.schema.json` | limits.run.maxDuration is whole hours, minutes or seconds. | `testdata/invalid/duration-not-a-duration.yaml` |
| `request-cap-zero` | `schema/template.schema.json` | limits.run.maxModelRequests is at least 1 when set. | `testdata/invalid/request-cap-zero.yaml` |
| `spend-cap-zero` | `schema/template.schema.json` | limits.run.maxSpendUSD is above zero when set. | `testdata/invalid/spend-cap-zero.yaml` |
| `spend-as-float` | `schema/template.schema.json` | limits.spendUSD is a string, not a number. | `testdata/invalid/spend-as-float.yaml` |
| `spend-three-decimals` | `schema/template.schema.json` | limits.spendUSD has at most two fraction digits. | `testdata/invalid/spend-three-decimals.yaml` |
| `resources-unknown-field` | `schema/template.schema.json` | spec.resources names only cpu and memory. | `testdata/invalid/resources-unknown-field.yaml` |
| `resources-bad-quantity` | `schema/template.schema.json` | A resource value is a Kubernetes quantity. | `testdata/invalid/resources-bad-quantity.yaml` |
| `questions-zero-duration` | `schema/template.schema.json` | A question timer is above zero. | `testdata/invalid/questions-zero-duration.yaml` |
| `notify-user` | `schema/template.schema.json` | A trigger's notify does not name a person. | `testdata/invalid/notify-user.yaml` |
| `notify-channel` | `schema/template.schema.json` | A trigger's notify does not name a chat channel. | `testdata/invalid/notify-channel.yaml` |
| `trigger-id-slug` | `schema/template.schema.json` | A trigger ID is a lowercase slug. | `testdata/invalid/trigger-id-slug.yaml` |
| `triggers-over-cap` | `schema/template.schema.json` | A template has at most 16 triggers. | `testdata/invalid/triggers-over-cap.yaml` |
| `tool-name-bad` | `schema/template.schema.json` | A spec.tools entry is a snake_case tool name. | `testdata/invalid/tool-name-bad.yaml` |
| `duration-over-cap` | `Template.check` | limits.run.maxDuration is at most 24h. | `testdata/invalid/duration-over-cap.yaml` |
| `spend-zero` | `Template.check` | limits.spendUSD is above zero. | `testdata/invalid/spend-zero.yaml` |
| `duplicate-connector` | `Template.check` | A template holds one grant per connector. | `testdata/invalid/duplicate-connector.yaml` |
| `targets-min-over-max` | `Template.check` | A grant's targets.min is at most targets.max. | `testdata/invalid/targets-min-over-max.yaml` |
| `resources-zero` | `Template.check` | A resource quantity is above zero. | `testdata/invalid/resources-zero.yaml` |
| `resources-request-over-limit` | `Template.check` | A resource request is at most its limit. | `testdata/invalid/resources-request-over-limit.yaml` |
| `questions-reminder-after-expiry` | `Template.check` | The question reminder comes before the expiry. | `testdata/invalid/questions-reminder-after-expiry.yaml` |
| `questions-expiry-over-cap` | `Template.check` | A question timer is at most 30 days. | `testdata/invalid/questions-expiry-over-cap.yaml` |
| `questions-reminder-under-min` | `Template.check` | A question timer is at least one minute. | `testdata/invalid/questions-reminder-under-min.yaml` |
| `trigger-duplicate-id` | `Template.check` | Trigger IDs are unique in a template. | `testdata/invalid/trigger-duplicate-id.yaml` |
| `trigger-cron-five-fields` | `Template.check` | A schedule's cron has five fields and no timezone prefix. | `testdata/invalid/trigger-cron-five-fields.yaml` |
| `trigger-timezone-unknown` | `Template.check` | A schedule's timezone is an IANA zone. | `testdata/invalid/trigger-timezone-unknown.yaml` |
| `trigger-direct-filter` | `Template.check` | A direct_message trigger has no filter. | `testdata/invalid/trigger-direct-filter.yaml` |
| `trigger-two-direct` | `Template.check` | A template has one direct_message trigger. | `testdata/invalid/trigger-two-direct.yaml` |
| `trigger-reserved-type` | `Template.check` | A trigger type reserved for v0.2 is refused. | `testdata/invalid/trigger-reserved-type.yaml` |
| `tool-undeclared` | `Template.check` | A tool the instructions call in backticks is listed in spec.tools. | `testdata/invalid/tool-undeclared.yaml` |
| `default-harness-not-allowed` | `Template.check` | spec.harness.default is one of spec.harness.allowed. | built value in `check_internal_test.go` |
| `plugin-installs-unsupported` | `Template.check` | pluginInstalls needs an allowed harness that supports it. | built value in `check_internal_test.go` |
| `file-name-mismatch` | `Load` | A file is named after its metadata.id. | `testdata/load/file-name-mismatch.yaml` |
| `unknown-connector` | `Template.CheckCatalog` | A grant names a connector type that exists. | `testdata/catalog/unknown-connector.yaml` |
| `unknown-scope` | `Template.CheckCatalog` | A grant's scopes exist on its connector type. | `testdata/catalog/unknown-scope.yaml` |
| `unknown-tool` | `Template.CheckCatalog` | A spec.tools entry is a tool a connector, the platform or a declaration names. | `testdata/catalog/unknown-tool.yaml` |
| `missing-max-model-requests` | `Template.CheckShipped` | A shipped template sets limits.run.maxModelRequests. | `testdata/shipped/missing-max-model-requests.yaml` |
| `missing-max-spend` | `Template.CheckShipped` | A shipped template sets limits.run.maxSpendUSD. | `testdata/shipped/missing-max-spend.yaml` |
| `run-spend-over-spend` | `Template.CheckShipped` | limits.run.maxSpendUSD is at most limits.spendUSD. | `testdata/shipped/run-spend-over-spend.yaml` |
| `missing-targets` | `Template.CheckShipped` | A shipped grant sets targets. | `testdata/shipped/missing-targets.yaml` |

`spec.harness` holds harness settings, all off when omitted. `harness.pluginInstalls: true` lets an
OpenCode agent install plugins: its sandbox may read the npm registry (GET and HEAD, no credentials)
from the OpenCode binary, and OpenCode loads external plugins (`harnesses/opencode/README.md`). It
needs `opencode` among the allowed harnesses. `repo-helper` sets it to `false` explicitly.

`spec.tools` (optional) lists the tool names the instructions expect the agent to call, also the ones
they only describe in prose ("the search and fetch tools"), for example `[ask_user, web_url_read]`.
It names tools, not MCP servers: an admin allows servers on the agent. `Parse` refuses a tool the
instructions name in backticks that the list lacks (rule `tool-undeclared`), `CheckCatalog` refuses a name no
connector, platform tool or declaration knows, and the legs check reads the list.

`spec.ephemeral: true` makes agents created from the template ephemeral: each task runs in a fresh
sandbox with no persistent home, deleted when the task ends (ADR 0065 decision 13c). Off when
omitted. Whoever creates the agent can change it.

`spec.questions` (optional) sets `reminderAfter` (default `24h`) and `expireAfter` (default `168h`, 7
days) for the questions the agent asks with `ask_user` (ADR 0065 decision 10): the person asked
gets an in-app reminder, and at expiry the question's safe default applies and the agent gets one
"no answer" run. Each is between 1m and 720h, and the reminder must come before the expiry.

Bump `metadata.version` whenever you change a template's content. Scopes are checked against the
connector type by `CheckCatalog`, not by the schema (see Rules).

`spec.triggers` (optional) lists what starts work on agents made from the template (ADR 0066):
`{id, type, filter, instruction}`. `type` is `schedule` (`filter: {cron: "0 9 * * 1-5", timezone:
Europe/Tallinn}`, five cron fields and an IANA zone) or `direct_message` (no filter); `code_host_event`
and `channel_event` are reserved for v0.2. The filter is typed per type; there is no expression
language. The triggers are copied into the agent's first version and fire only once an admin or
owner activates a version that holds them.

A template may have no grants (`grants: []`): `finops-analyst` works only through the MCP servers an admin
allows on the agent's page, which are not template grants.
A scheduled task has no creator, so `ask_user` cannot ask one. The templates call
`ask_user` without `to`, and Koinon asks the trigger's notify target, by default the org's admins.
The admin picks the notify person by name on Edit triggers, so the bootstrap admin, who has no
email, needs nothing extra. The trigger instructions carry no `Ask:` line. `lcm-upgrader`'s trigger
instruction also has a `Repository:` line that the admin sets on Edit triggers.
