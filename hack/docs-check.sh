#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks that the docs site (docs/site) still matches the code (plan item DOC1, ADR 0108). Needs bash,
# awk, grep and helm; it never talks to a cluster and never builds the site (make docs does that).
#
#   1. The command files (test/e2e/commands/*.e1) are the source of the quickstart's and the backup
#      guide's commands: they parse (bash -n), and the `sh e1=<name>` blocks of the docs pages equal
#      them. The page follows the file, never the other way round.
#   2. test/e2e/run.sh runs every block of quickstart.e1, and only blocks that exist.
#   2b. test/e2e/backup-restore.sh runs exactly the blocks of backup-restore.e1.
#   3. Every `yaml chart-values` block in the docs is valid chart values (helm lint --strict).
#   3b. Every `yaml model-provider` block, and the model-provider manifests of the e2e test and the dev
#      profiles, parse as declarative Secrets (internal/declarative, TestDocumentedSecretsParse), and
#      so do the `yaml mcp-server` blocks of the MCP servers guide.
#   4. Every bold label in the quickstart's browser steps appears in the Playwright spec as a whole
#      quoted string or regexp.
#   5. The versions on the Prerequisites page equal the ones test/e2e/kind/up.sh, the preflight,
#      the chart and the e2e workflow use.
#   6. Every repository file the docs link to exists.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
docs=docs/site/src/content/docs
quickstart=$docs/quickstart.md
cmds=test/e2e/commands
HELM=${HELM:-helm}

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
ok() { echo "ok: $*"; }

# --update used to rewrite the golden file from the page. The command file is now the source and is
# edited by hand, so there is nothing to generate: the flag is an error, not a second mode.
if [[ ${1:-} == --update ]]; then
  echo "docs-check: --update is gone, there is nothing to generate. Edit test/e2e/commands/*.e1, then make the docs page match, then run 'make docs-check'." >&2
  exit 1
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/docs-check.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# same <command file> <page> <what>: the page's e1 blocks must equal the command file's.
# Plain files, not process substitution or `diff -`: sandboxes (macOS Seatbelt) deny opening
# /dev/fd/N and /dev/stdin, so a pipe into diff fails there as if the files differed.
same() {
  local file=$1 page=$2
  E1_FILE=$file hack/docs-e1.sh script >"$tmp/file.script" || { fail "reading the e1 blocks of $file"; return; }
  E1_DOC=$page hack/docs-e1.sh script >"$tmp/page.script" || { fail "reading the e1 blocks of $page"; return; }
  [[ -s $tmp/file.script ]] || { fail "$file has no e1 blocks"; return; }
  if diff -u "$tmp/file.script" "$tmp/page.script" >"$tmp/page.diff"; then
    ok "the e1 blocks of $page equal $file"
  else
    cat "$tmp/page.diff" >&2
    fail "the e1 blocks of $page differ from $file (the file is what test/e2e runs and what koinon-docs shows; change the page to match, or change both on purpose and check the e2e flow)"
  fi
}

# parses <command file>: every block is valid shell.
parses() {
  local file=$1 n
  for n in $(E1_FILE=$file hack/docs-e1.sh names); do
    E1_FILE=$file hack/docs-e1.sh show "$n" >"$tmp/$n.sh" || { fail "e1 block $n of $file is empty"; continue; }
    bash -n "$tmp/$n.sh" || fail "e1 block $n of $file is not valid shell"
  done
}

# 0. The offline tests of hack/docs-e1.sh (the reader of the command files).
if hack/docs-e1-test.sh >"$tmp/e1test.out" 2>&1; then
  ok "hack/docs-e1-test.sh passes"
else
  cat "$tmp/e1test.out" >&2
  fail "hack/docs-e1-test.sh fails"
fi

# 1. The command files and the pages that show them.
quickcmds=$cmds/quickstart.e1
backupcmds=$cmds/backup-restore.e1
guide=$docs/guides/backup-restore.md
names=$(E1_FILE=$quickcmds hack/docs-e1.sh names) || { fail "reading the e1 blocks of $quickcmds"; names=""; }
[[ -n $names ]] || fail "$quickcmds has no e1 blocks"
gnames=$(E1_FILE=$backupcmds hack/docs-e1.sh names) || { fail "reading the e1 blocks of $backupcmds"; gnames=""; }
[[ -n $gnames ]] || fail "$backupcmds has no e1 blocks"
parses "$quickcmds"
parses "$backupcmds"
same "$quickcmds" "$quickstart"
same "$backupcmds" "$guide"

# 2. run.sh runs exactly the e1 blocks.
used=$(grep -v '^[[:space:]]*#' test/e2e/run.sh | grep -oE '(\be1 |docs-e1\.sh" run )[a-z0-9-]+' | awk '{print $NF}' | sort -u)
for n in $used; do
  grep -qx "$n" <<<"$names" || fail "test/e2e/run.sh runs e1 block '$n', which $quickcmds does not have"
done
for n in $names; do
  grep -qx "$n" <<<"$used" || fail "test/e2e/run.sh does not run e1 block '$n'"
done
[[ $failures -eq 0 ]] && ok "test/e2e/run.sh runs every e1 block: $(tr '\n' ' ' <<<"$used")"

# 2b. test/e2e/backup-restore.sh (the nightly step, #90) runs exactly the blocks of the backup file.
gused=$(grep -v '^[[:space:]]*#' test/e2e/backup-restore.sh | grep -oE '\bblk [a-z0-9-]+' | awk '{print $NF}' | sort -u)
for n in $gused; do
  grep -qx "$n" <<<"$gnames" || fail "test/e2e/backup-restore.sh runs e1 block '$n', which $backupcmds does not have"
done
for n in $gnames; do
  grep -qx "$n" <<<"$gused" || fail "test/e2e/backup-restore.sh does not run e1 block '$n' of $backupcmds"
done
ok "test/e2e/backup-restore.sh runs every e1 block of the backup file: $(tr '\n' ' ' <<<"$gused")"

# 3. Chart values blocks. The block list goes through a file (see the command-file check above).
for d in "$docs"/*.md "$docs"/guides/*.md; do
  awk -v dir="$tmp" -v base="$(basename "$d" .md)" '
    /^```yaml chart-values/ { n++; out = sprintf("%s/%s-%d.yaml", dir, base, n); printf "" > out; inblock = 1; next }
    /^```/ { if (inblock) { close(out); printf "%s%c", out, 0; inblock = 0 } ; next }
    inblock { print > out }
  ' "$d"
done >"$tmp/blocks.list"
i=0
while IFS= read -r -d '' f; do
  i=$((i + 1))
  # A block that is an overlay (it does not choose between openshell.clientTLSSecret and
  # openshell.insecure) is checked as if the base block's choice were made; likewise for
  # openshell.address, which the chart requires.
  tls=(--set openshell.insecure=true)
  if grep -qE 'clientTLSSecret|insecure' "$f"; then tls=(); fi
  if ! grep -qE '^\s+address:' "$f"; then tls+=(--set openshell.address=openshell.openshell.svc:8080); fi
  if "$HELM" lint --strict charts/koinon -f "$f" ${tls[@]+"${tls[@]}"} >"$tmp/lint.out" 2>&1 && \
    "$HELM" template koinon charts/koinon --namespace koinon -f "$f" ${tls[@]+"${tls[@]}"} >/dev/null 2>"$tmp/tpl.out"; then
    :
  else
    cat "$tmp/lint.out" "$tmp/tpl.out" >&2
    fail "chart-values block $(basename "$f") is not valid chart values"
  fi
done <"$tmp/blocks.list"
[[ $i -gt 0 ]] || fail "no chart-values blocks found"
ok "$i chart-values blocks checked with helm lint and template"

# 3b. Declarative Secrets shown in the docs.
if go test -count=1 -run 'TestDocumentedSecretsParse|TestDocumentedMCPServerSecretsParse' ./internal/declarative >"$tmp/secrets.out" 2>&1; then
  ok "the model-provider and mcp-server manifests in the docs, e2e test and dev profiles parse"
else
  cat "$tmp/secrets.out" >&2
  fail "a model-provider manifest does not parse; see internal/declarative/docs_test.go"
fi

# 4. UI labels.
spec=$(cat web/e2e-real/*.spec.ts)
awk '/^## 3\./ {on=1} /^## Changing in v0.1/ {on=0} on' "$quickstart" | grep -oE '\*\*[^*]+\*\*' | sort -u | sed -e 's/^\*\*//' -e 's/\*\*$//' >"$tmp/labels"
[[ -s $tmp/labels ]] || fail "no bold UI labels found in the quickstart's browser steps"
while IFS= read -r label; do
  # The label must be a whole quoted string ('Label') or a regexp ( /Label/ ) in the spec.
  grep -qF -e "'$label'" -e "/$label/" <<<"$spec" || fail "the quickstart names '$label', which web/e2e-real/*.spec.ts does not contain"
done <"$tmp/labels"
ok "$(wc -l <"$tmp/labels" | tr -d ' ') UI labels are in the Playwright spec"

# 5. Pins.
up=test/e2e/kind/up.sh
pre=$docs/prerequisites.md
pin() { # pin <file> <variable>: the value assigned to a variable in up.sh
  sed -n "s/^$2=\\(.*\\)\$/\\1/p" "$1" | head -1
}
need() { # need <page> <what> <string>
  [[ -n $3 ]] || { fail "could not read $2"; return; }
  grep -qF -- "$3" "$1" || fail "$1 does not contain $2 ($3)"
}
for v in AS_VERSION AS_SHA256 OS_VERSION OS_DIGEST KNP_VERSION KNP_SHA256 CNPG_VERSION CNPG_SHA256 CNPG_IMAGE_DIGEST; do
  need "$pre" "$v from $up" "$(pin $up $v)"
done
node=$(pin $up NODE_IMAGE)
need "$pre" "NODE_IMAGE from $up" "$node"
need "$pre" "Kubernetes version of the node image" "$(sed -n 's/.*node:v\([0-9]*\.[0-9]*\)\..*/\1/p' <<<"$node")"
need "$pre" "kubeVersion floor in Chart.yaml" "$(sed -n 's/^kubeVersion: *">=\([0-9]*\.[0-9]*\)\..*/\1/p' charts/koinon/Chart.yaml)"
need "$pre" "agent sandbox version in the preflight" "$(sed -n 's#.*agent-sandbox/releases/download/\(v[0-9.]*\)/.*#\1#p' internal/preflight/preflight.go | head -1)"
need "$pre" "OpenShell version in the preflight" "$(sed -n 's/.*SupportedOpenShellVersions = \[\]string{"\([^"]*\)"}.*/\1/p' internal/preflight/preflight.go)"
need "$pre" "Postgres image tag in values.yaml" "$(sed -n 's#.*postgresql:\([0-9][0-9.]*\)-.*#\1#p' charts/koinon/values.yaml | head -1)"
need "$pre" "kind version in hack/versions.env" "$(pin hack/versions.env KIND_VERSION)"
need "$pre" "Helm version in hack/versions.env" "$(pin hack/versions.env HELM_VERSION | sed 's/^v//')"
need "$docs/guides/gateway-modes.md" "agentgateway tag in values.yaml" "$(sed -n 's/^ *tag: \(v1\.[0-9.]*\)$/\1/p' charts/koinon/values.yaml | head -1)"
[[ $failures -eq 0 ]] && ok "prerequisite versions match the repository"

# 6. Links into the repository.
grep -rhoE 'github\.com/stateracloud/koinon/blob/main/[A-Za-z0-9_./-]+' "$docs" | sort -u | while read -r url; do
  p=${url#github.com/stateracloud/koinon/blob/main/}
  [[ -e $p ]] || echo "FAIL: the docs link to $p, which does not exist" >&2
done >"$tmp/links.out" 2>&1 || true
if [[ -s $tmp/links.out ]]; then cat "$tmp/links.out" >&2; fail "broken repository links"; else ok "repository links resolve"; fi

if [[ $failures -ne 0 ]]; then
  echo "docs-check: $failures problem(s)" >&2
  exit 1
fi
echo "docs-check: all checks passed"
