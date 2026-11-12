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
  `spec.tools`, classified with the declarations in `testdata/legs/tool-declarations.json`. A drift
  check fails when the instructions name a tool in backticks that `spec.tools` does not list. To add
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

`spec.harness` holds harness settings, all off when omitted. `harness.pluginInstalls: true` lets an
OpenCode agent install plugins: its sandbox may read the npm registry (GET and HEAD, no credentials)
from the OpenCode binary, and OpenCode loads external plugins (`flavors/opencode/README.md`). It
needs `opencode` among the allowed harnesses. `repo-helper` sets it to `false` explicitly.

`spec.tools` (optional) lists the tool names the instructions expect the agent to call, also the ones
they only describe in prose ("the search and fetch tools"), for example `[ask_user, web_url_read]`.
It names tools, not MCP servers: an admin allows servers on the agent. The legs check reads it.

`spec.ephemeral: true` makes agents created from the template ephemeral: each task runs in a fresh
sandbox with no persistent home, deleted when the task ends (ADR 0065 decision 13c). Off when
omitted. Whoever creates the agent can change it.

`spec.questions` (optional) sets `reminderAfter` (default `24h`) and `expireAfter` (default `168h`, 7
days) for the questions the agent asks with `ask_user` (ADR 0065 decision 10): the person asked
gets an in-app reminder, and at expiry the question's safe default applies and the agent gets one
"no answer" run. Each is between 1m and 720h, and the reminder must come before the expiry.

Bump `metadata.version` whenever you change a template's content. Scopes are checked against the
connector type when templates are synced to the database, not by the schema.

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
