# ast-grep trial: code-shape questions, grep against ast-grep

- Status: Complete
- Date: 2026-11-02
- Area: engineering-process
- Tags: tooling, testing

Research date: 2026-11-02, for #528. One cloud session ran four real code-shape questions on this
repository at `main` (`dc1ebc4`), each once with grep (and the shell tools an agent reaches for next)
and once with ast-grep, and compared tool calls, output characters, true and false positives and
whether an ast-grep rewrite (dry run only) would have been safe. No code was changed.

## Short answer

**Skip for now: no AGENTS.md line and no rule set.** ast-grep answers structural questions that
grep cannot (tasks b, c and d2 below) with output 15 to 50 times smaller. But on these four tasks
it found no bug that grep or an existing linter misses. Each of its non-trivial rules first returned
a silent zero: a wrong pattern looks the same as a clean tree. Its rewrites do not manage imports or
check what is in scope. The repeatable checks among these tasks already belong to golangci-lint (`noctx`,
`go vet`'s `testinggoroutine`), kubeconform and the chart assertions. Revisit when a recurring
check needs a shape that no existing linter expresses; then land it as a rule with a fixture test in
CI, not as an ad-hoc agent search.

No real bugs found: no production request without a context, no assertion in a goroutine without
`GinkgoRecover`, no RPC handler leaking a raw error, no unpinned action and no container without
`resources` in any CI render of the chart.

## Method

- **ast-grep 0.51.0** (`@ast-grep/cli` on npm, published 2026-10-15, 18 days before the trial; ADR
  0079's 3-day rule holds). Installed with `npm install -g @ast-grep/cli@0.51.0`. The Debian `sg`
  on the PATH is shadow-utils' `sg`, not ast-grep: call it as `ast-grep`.
- **CodeGraph was not installed in this session**, so the baseline is grep, `sed -n`, `jq` and a
  YAML parser. CodeGraph would help with task c's caller tracing, not with the shape matching.
- **Calls** count the tool calls until the answer was trusted, including failed attempts and the
  fixture runs. **Output** is the characters of the result the model read. Ground truth comes from
  reading the code (a, b, c) or from parsing the YAML with Python (d2).
- Each ast-grep rule was checked against a small fixture with planted positives and negatives before
  its zero was trusted. The rules are in the appendix.

## Results

| Task | grep: calls, output, hits | ast-grep: calls, output, hits | True positives | Rewrite (dry run) |
|---|---|---|---|---|
| (a) `http.NewRequest` without a context | 1 call, 481 chars, 4 | 4 calls (first pattern a silent zero), 473 chars, 4 | 4, all in tests; 0 in production | Not safe: Ginkgo specs have no `t` for `t.Context()`, and `context.Background()` needs an import that ast-grep does not add |
| (b) `t.Fatal`-family, or `Expect` without `defer GinkgoRecover()`, inside a goroutine in tests | 2 calls, 5.1k chars (52k with the `-A12` context), 58 candidate lines | 5 calls (two rules, two fixtures, one parent-chain fix), about 100 chars, 0 | 0; all 58 grep lines are false positives (outside the goroutine or after `GinkgoRecover`) | n/a |
| (c) RPC handler returning a raw `err` (leaks its text as `CodeUnknown`) | 2 calls, 10.5k chars (201 `return nil, err` lines, helpers included), or 62k with `-B3` to see the source of `err` | 3 calls for the shape; grouped by callee, 329 chars (16 callees, 158 sites), plus 7 sites not after an assignment; 165 in all | 0: every callee returns a connect error (`authorize`, `parseID`, `*Caller`, `checkMessage`, ...), traced by hand in 8 more calls | Not applicable: it needs data flow across functions, which neither tool has |
| (d1) workflow `uses:` not pinned to a SHA | 1 call, 13k chars to see all 115, 0 after the filter | 2 calls, 0 | 0 (114 `uses:` pairs, all pinned) | n/a |
| (d2) chart container without `resources` (7 CI renders, 45 containers) | not feasible with grep (indentation); 1 Python call as ground truth | 4 calls (first rule a silent zero), 0 | 0 | n/a |

## What the trial showed

- **Where ast-grep wins.** Containment questions ("inside a goroutine", "inside an RPC handler",
  "an item of `containers` lacking a key") come back as exact hits. grep returns candidate lines
  that each need reading: 58 lines for b, all false positives. `--json` piped through `jq` lets you
  group results (task c: 158 sites as 16 callee names in 329 characters).
- **Silent zeros are the main hazard.** In Go, `pkg.F($$$)` parses as an `ERROR` node (a qualified
  type) and matches nothing: `fmt.Errorf($$$)` gives 0 against grep's 960. It only works as
  `pattern: {context: "func _() { fmt.Errorf($$$) }", selector: call_expression}` or with a leading
  fixed argument (`http.NewRequest($A, $$$)`). In YAML a sequence item sits under `block_sequence`
  and `block_node`, so a naive `inside`/`has` chain also matches nothing. Every rule needs a fixture
  with a planted positive before an agent can trust its zero; an agent that skips this reports
  "none found" wrongly.
- **Raw JSON is large.** `--json` carries the whole matched node and its context: 800k characters for
  task c's 158 hits. Always project with `jq`; the default text output (117k for task c) is no better.
- **No data flow.** Task c's real question, where the `err` came from, was answered by reading the
  helpers. ast-grep only narrowed the shape (`follows` an assignment from a given callee).
- **Rewrites are not type- or scope-aware.** Task a's rewrite looked correct in the diff and would
  not compile in the Ginkgo files. `goimports` and a build would have to follow any rewrite.
- **Existing linters already cover the recurring cases.** `noctx` is enabled in `.golangci.yml`;
  `go vet` (in golangci-lint's standard set) runs `testinggoroutine`, which is task b's `t.Fatal`
  half; actions are pinned and chart renders are checked by `hack/chart-check.sh` and kubeconform.
- **Demand is low.** Issue #528 counted 10 search loops in two weeks of sessions, none a bulk rewrite.

## If it is adopted later

Use one AGENTS.md line: "Code-shape checks that no linter covers: an ast-grep rule under
`hack/ast-grep/` with a fixture test, run in CI; prove a zero with a planted positive." The candidate
rule from this trial is b2, `Expect` in a goroutine without `GinkgoRecover`, because no enabled
linter covers it.

## Appendix: the rules

```yaml
# (b) t.Fatal-family inside a goroutine (go vet's testinggoroutine covers this already)
id: t-fatal-in-goroutine
language: go
files: ["**/*_test.go"]
rule:
  kind: call_expression
  has:
    field: function
    kind: selector_expression
    regex: '^\w+\.(Fatal|Fatalf|FailNow|Skip|Skipf|SkipNow)$'
  inside: { kind: go_statement, stopBy: end }
---
# (b2) a Gomega assertion in `go func(){...}()` without defer GinkgoRecover()
id: expect-in-goroutine-without-recover
language: go
files: ["**/*_test.go"]
rule:
  kind: call_expression
  has: { field: function, regex: '^(Expect|Ω|Fail|Eventually|Consistently)$' }
  inside:
    kind: func_literal
    stopBy: end
    inside: { kind: call_expression, inside: { kind: go_statement } }
    not: { has: { stopBy: end, pattern: defer GinkgoRecover() } }
---
# (c) `if err != nil { return nil, err }` right after an assignment, in an RPC handler;
# group the hits by $CALLEE with jq
id: rpc-raw-err
language: go
ignores: ["**/*_test.go"]
rule:
  kind: if_statement
  pattern: "if err != nil { return nil, err }"
  follows:
    any: [{ kind: short_var_declaration }, { kind: assignment_statement }]
    has:
      kind: expression_list
      field: right
      has: { kind: call_expression, has: { field: function, pattern: $CALLEE } }
  inside:
    kind: method_declaration
    stopBy: end
    has: { field: result, regex: 'Response, error\)$' }
---
# (d1) a workflow action not pinned to a commit SHA
id: action-not-pinned
language: yaml
files: [".github/**/*.yml"]
rule:
  kind: block_mapping_pair
  has: { field: key, regex: "^uses$" }
  not: { has: { field: value, regex: '@[0-9a-f]{40}$|^\./|^docker://' } }
---
# (d2) a container without resources, on `helm template` output (templates are not YAML)
id: container-without-resources
language: yaml
rule:
  kind: block_sequence_item
  inside:
    kind: block_sequence
    inside:
      kind: block_node
      inside:
        kind: block_mapping_pair
        has: { field: key, regex: "^(containers|initContainers)$" }
  has:
    kind: block_node
    has:
      kind: block_mapping
      not: { has: { kind: block_mapping_pair, has: { field: key, regex: "^resources$" } } }
```
