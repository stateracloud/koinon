#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/drill-issue.sh with a fake gh: an open drill-red issue gets a comment, none gets a new issue.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/drill-issue-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# The fake gh logs its arguments; `issue list` prints $OPEN (an issue number, or nothing).
cat >"$tmp/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$CALLS"
case "$1 $2" in
"issue list") printf '%s' "${OPEN:-}" ;;
"issue create") echo "https://example.invalid/issues/7" ;;
esac
EOF
chmod +x "$tmp/gh"

fail=0
check() { if grep -q -- "$2" <<<"$1"; then echo "ok: $3"; else echo "FAIL: $3 (missing '$2')" >&2; fail=1; fi; }
nocheck() { if grep -q -- "$2" <<<"$1"; then echo "FAIL: $3 (found '$2')" >&2; fail=1; else echo "ok: $3"; fi; }
run() {
  : >"$tmp/calls"
  GH="$tmp/gh" CALLS="$tmp/calls" GITHUB_RUN_URL=https://example.invalid/runs/1 GITHUB_SHA=0123456789abcdef \
    GITHUB_EVENT_NAME=schedule GITHUB_REF_NAME=main "$repo/hack/drill-issue.sh" "$@" 2>&1
  cat "$tmp/calls"
}

out=$(OPEN=42 run)
check "$out" "gh issue comment 42" "comments on the open issue"
nocheck "$out" "gh issue create" "and opens no second one"
check "$out" "runs/1" "the comment links the run"

out=$(OPEN="" run)
check "$out" "gh issue create --title Restore drill red on main (012345678)" "opens an issue when none is open"
check "$out" "--label drill-red" "labels it"
nocheck "$out" "gh issue comment" "and does not comment"

out=$(OPEN=42 run --dry-run)
check "$out" "would look for an open issue with label drill-red" "dry run says what it would do"
nocheck "$out" "gh issue" "dry run calls no gh"

exit "$fail"
