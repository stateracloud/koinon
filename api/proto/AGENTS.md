The root AGENTS.md applies too; this file adds rules for `api/proto/`.

- Only add fields. Never reuse, renumber or delete one: `make breaking` must pass (buf category `FILE`,
  checked against main). Lint the definitions with `make lint-proto`.
- Generated Go code from these files is committed; the root rule on `make generate` covers it.
