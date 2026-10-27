# templates

Agent templates: building blocks for the agent builder; the result is always a draft (ADR 0020).

- `<id>.yaml`: one template per file, named after `metadata.id`, `apiVersion:
  templates.koinon.dev/v1alpha1`, `kind: Template`.
- `schema/template.schema.json`: the JSON Schema (2020-12) every template must pass.
- `templates.go`: embeds the templates into the binary, validates them (schema plus the rules a
  schema cannot express, such as the default flavor being allowed) and computes a content hash over
  the canonical JSON form, so comments and formatting do not change it.
- `testdata/`: a minimal valid template and invalid fixtures; each invalid file names the error it
  must produce on a `# want:` line.

`spec.harness` holds harness settings, all off when omitted. `harness.pluginInstalls: true` lets an
OpenCode agent install plugins: its sandbox may read the npm registry (GET and HEAD, no credentials)
from the OpenCode binary, and OpenCode loads external plugins (`flavors/opencode/README.md`). It
needs `opencode` among the allowed flavors. `repo-helper` sets it to `false` explicitly.

`spec.ephemeral: true` makes agents created from the template ephemeral: each task runs in a fresh
sandbox with no persistent home, deleted when the task ends (ADR 0065 decision 13c). Off when
omitted. Whoever creates the agent can change it.

Bump `metadata.version` whenever you change a template's content. Scopes are checked against the
connector type when templates are synced to the database, not by the schema.
