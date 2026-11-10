#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The merge queue (#225) builds a commit that belongs to no pull request and waits for every
# required check on it. A workflow that produces a required context but does not listen to
# `merge_group` leaves that check absent, and the queue entry waits until it times out. This test
# keeps the workflows that produce a required context (hack/ci-required.txt) subscribed, and keeps
# the jobs that read pull request data from running on a queue build.

# The check commands are single-quoted on purpose: bash -c expands $1 and $2 itself.
# shellcheck disable=SC2016

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wfdir=${1:-$repo/.github/workflows}

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }
# The workflows that produce a required context: ci.yml (the jobs), and the external ones named in the
# header of hack/ci-required.txt. That they listen to merge_group at all is checked by hack/ci-required-test.sh;
# here the trigger must also ask for checks_requested only.
while IFS= read -r wf; do
	# shellcheck disable=SC2016
	check "$wf asks for checks_requested only" awk '/^  merge_group:/ { getline; if ($0 ~ /types: \[checks_requested\]/) ok = 1 } END { exit !ok }' "$wfdir/$wf"
done < <({ echo ci.yml; sed -n -E 's@^#[[:space:]]+[^[:space:]].*[[:space:]]\.github/workflows/([A-Za-z0-9_.-]+\.yml)[[:space:]]*$@\1@p' "$repo/hack/ci-required.txt"; } | sort -u)
# The jobs that are not part of a queue build stay off it: they read a pull request or push to main.
# shellcheck disable=SC2016
check "claims.yml: release does not run on merge_group" bash -c '
	! awk "/^  release:/ { j = 1 } /^  claim:/ { j = 0 } j && /merge_group/ { bad = 1 } END { exit !bad }" "$1"' _ "$wfdir/claims.yml"
check "claims.yml: the claim job reports on merge_group" grep -Eq "if: github.event_name == 'merge_group' \|\|" "$wfdir/claims.yml"
# shellcheck disable=SC2016
check "claims.yml: on merge_group the claim job runs check-pr for the queued PR from the base sha" bash -c '
	grep -q "ref: \${{ github.event.merge_group.base_sha }}" "$1" && grep -q "gh-readonly-queue/\*/pr-" "$1" &&
	awk "/event_name == .merge_group.$/ { g = 1 } g && /claim.sh check-pr/ { ok = 1 } END { exit !ok }" "$1"' _ "$wfdir/claims.yml"
# On pull_request the claim job runs the base branch tip (base.ref), never the base sha recorded at the
# PR's last push, which can be many commits behind main.
# shellcheck disable=SC2016
check "claims.yml: on pull_request the claim job checks out the base branch tip" bash -c '
	grep -q "ref: \${{ github.event.pull_request.base.ref }}" "$1" && ! grep -q "pull_request.base.sha }}" "$1"' _ "$wfdir/claims.yml"
# #444: the queue commit runs the quickstart itself (decide answers needed); `e2e result` reads the
# queued PR's head only in the bootstrap shim (hack/e2e-queue-head.sh), and only when an OLD base's decide
# answered required=true on merge_group. The lab's e2e-lab status cannot test the queue ref.
# shellcheck disable=SC2016
check "e2e-kind.yml: e2e result looks up the queued PR head only through the guarded shim" bash -c '
	! grep -q "headRefOid" "$1" && ! grep -q "gh-readonly-queue" "$1" &&
	[ "$(grep -c "e2e-queue-head.sh" "$1")" -ge 2 ] &&
	grep -q "if \[\[ \$EVENT == merge_group && \$REQUIRED == true \]\]; then" "$1" &&
	grep -A1 "if \[\[ \$EVENT == merge_group && \$REQUIRED == true \]\]; then" "$1" | grep -q "hack/e2e-queue-head.sh"' _ "$wfdir/e2e-kind.yml"
check "e2e-kind.yml: the shim names its exit condition and the follow-up" grep -q "BOOTSTRAP SHIM (follow-up: #452)" "$wfdir/e2e-kind.yml"
# The shim: merge_group + required=true (only an old base's decide produces it) -> the PR-head lookup runs,
# and hack/e2e-gate.sh then judges that head. A fake gh answers `pr view`.
fakebin=$(mktemp -d "${TMPDIR:-/tmp}/ci-mg-test.XXXXXX") || exit 1
trap 'rm -rf "$fakebin"' EXIT
cat >"$fakebin/gh" <<'EOF'
#!/usr/bin/env bash
# fake gh: `pr view N --json headRefOid|headRefName --jq ...`
case "$*" in
*headRefOid*) echo "${FAKE_HEAD_SHA-0123456789abcdef0123456789abcdef01234567}" ;;
*headRefName*) echo "ci/444-fake-branch" ;;
esac
EOF
chmod +x "$fakebin/gh"
shim="$repo/hack/e2e-queue-head.sh"
export GH="$fakebin/gh"
check "shim: reads the PR head and branch from the queue ref" bash -c '
	out=$(GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-444-aaaa "$1" 2>/dev/null) &&
	grep -q "^SHA=0123456789abcdef0123456789abcdef01234567$" <<<"$out" && grep -q "^BRANCH=ci/444-fake-branch$" <<<"$out"' _ "$shim"
check "shim: fails closed on a ref without a PR number" bash -c '! GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-x-aaaa "$1" 2>/dev/null' _ "$shim"
check "shim: fails closed when the PR head is empty" bash -c '! FAKE_HEAD_SHA= GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-444-aaaa "$1" 2>/dev/null' _ "$shim"
# Old-style decide on merge_group (needed=false, required=true, quickstart skipped): without a head the gate
# fails (the bug), with the shim's head and an e2e-lab success it passes.
gate="$repo/hack/e2e-gate.sh"
labgh="$fakebin/labgh"
cat >"$labgh" <<'EOF'
#!/usr/bin/env bash
case "$*" in
*"run list"*) ;;
*"/statuses"*) echo '[{"context":"e2e-lab","state":"success"}]' ;;
esac
EOF
chmod +x "$labgh"
check "gate: merge_group + required=true without a looked-up head fails closed" bash -c '! DECIDE=success QUICKSTART=skipped REQUIRED=true SHA= GH_REPO=x/y GH="$2" "$1"' _ "$gate" "$labgh"
# shellcheck disable=SC2016
check "gate: merge_group + required=true with the shim's head and e2e-lab success passes" bash -c '
	DECIDE=success QUICKSTART=skipped REQUIRED=true SHA=0123456789abcdef0123456789abcdef01234567 BRANCH=b GH_REPO=x/y GH="$2" "$1"' _ "$gate" "$labgh"
unset GH
# shellcheck disable=SC2016
check "e2e-kind.yml: the quickstart job is not limited to pull requests and pushes" bash -c '
	! awk "/^  quickstart:/ { j = 1 } /^  result:/ { j = 0 } j && /^    if:.*event_name/ { bad = 1 } END { exit !bad }" "$1"' _ "$wfdir/e2e-kind.yml"
# shellcheck disable=SC2016
check "e2e-kind.yml: a queue build is never cancelled" bash -c '
	grep -q "cancel-in-progress: \${{ github.event_name == .pull_request. }}" "$1"' _ "$wfdir/e2e-kind.yml"

# Every required name reports on a queue build. Only a workflow without a `merge_group` trigger makes the entry wait
# for the 60-minute timeout; a job skipped by its `if:` reports a skipped (green) check that tests nothing, which is
# also wrong for a required name, so (b) and (c) catch that. For every name in hack/ci-required.txt (not the advisory ones) plus
# `review` (ADR 0103): find the job that produces it (its `name:`), then check (a) the workflow listens to
# `merge_group`, (b) the job's `if:` is not false on merge_group, (c) every job it `needs:` passes (b) too, unless
# the job runs under always()/!cancelled(). The `if:` forms are a small table (event_name compares, the draft flag,
# always(), !cancelled(), outputs and results of other jobs); an unknown form fails the test, so teach it the new form.
mg_eval() { # mg_eval <workflow dir> <name> -> prints "ok <workflow>/<job>", "FAIL <reason>" or "MISSING <reason>"
	python3 -I - "$1" "$2" <<'PY'
import os, re, sys
wfdir, want = sys.argv[1], sys.argv[2]

def parse(path):
    """-> (listens_merge_group, {job_id: {"name","if","needs"}}) from the workflow text, line based."""
    lines = open(path).read().split("\n")
    mg, jobs, cur, in_jobs, in_on = False, {}, None, False, False
    i = 0
    while i < len(lines):
        l = lines[i]
        if re.match(r"^on:", l):
            in_on = True
        elif re.match(r"^[A-Za-z_-]+:", l):
            in_on = False
            in_jobs = l.startswith("jobs:")
        if in_on and re.match(r"^  merge_group:", l):
            mg = True
        if in_jobs:
            m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", l)
            if m:
                cur = {"name": m.group(1), "if": "", "needs": []}
                jobs[m.group(1)] = cur
            elif cur is not None:
                m = re.match(r"^    name:\s*(.*?)\s*$", l)
                if m:
                    cur["name"] = m.group(1).strip("\"'")
                m = re.match(r"^    if:\s*(.*?)\s*$", l)
                if m:
                    v = m.group(1)
                    if v in (">-", ">", "|", "|-"):
                        parts = []
                        while i + 1 < len(lines) and re.match(r"^      \S", lines[i + 1]):
                            i += 1
                            parts.append(lines[i].strip())
                        v = " ".join(parts)
                    cur["if"] = v
                m = re.match(r"^    needs:\s*(.*?)\s*$", l)
                if m:
                    v = m.group(1)
                    if v:
                        cur["needs"] = [x.strip() for x in v.strip("[]").split(",") if x.strip()]
                    else:
                        while i + 1 < len(lines) and re.match(r"^      - ", lines[i + 1]):
                            i += 1
                            cur["needs"].append(lines[i].strip()[2:].strip())
        i += 1
    return mg, jobs

ATOMS = [
    (r"github\.event_name\s*==\s*'([a-z_]+)'", lambda g: g == "merge_group"),
    (r"github\.event_name\s*!=\s*'([a-z_]+)'", lambda g: g != "merge_group"),
    (r"github\.event\.pull_request\.draft", lambda: False),
    (r"always\(\)", lambda: True),
    (r"!cancelled\(\)", lambda: True),
    # conditional on an upstream output or result: a skip still reports, so it counts as running
    (r"needs\.[A-Za-z0-9_-]+\.(?:outputs\.[A-Za-z0-9_-]+|result)\s*(?:==|!=)\s*'[^']*'", lambda: True),
]

def runs_on_mg(cond):
    """Evaluate an `if:` for event_name == merge_group. Unknown forms raise ValueError."""
    c = cond.strip()
    if c == "":
        return True
    m = re.fullmatch(r"\$\{\{(.*)\}\}", c, re.S)
    if m:
        c = m.group(1).strip()
    tok = re.compile(r"\s*(&&|\|\||\(|\)|!(?!=|cancelled)|" + "|".join("(?:%s)" % a for a, _ in ATOMS) + ")")
    out, pos = [], 0
    while pos < len(c):
        t = tok.match(c, pos)
        if not t:
            raise ValueError("unknown if: form near %r in %r" % (c[pos:pos + 40], cond))
        text, pos = t.group(1), t.end()
        if text == "&&":
            out.append(" and ")
        elif text == "||":
            out.append(" or ")
        elif text in ("(", ")"):
            out.append(text)
        elif text == "!":
            out.append(" not ")
        else:
            for a, fn in ATOMS:
                mm = re.fullmatch(a, text)
                if mm:
                    out.append(str(fn(*mm.groups())))
                    break
    return bool(eval("".join(out), {"__builtins__": {}}))

def keeps_reporting(jobs, jid, seen=()):
    j = jobs[jid]
    if not runs_on_mg(j["if"]):
        return "job %s: if: is false on merge_group (%s)" % (jid, j["if"])
    if re.search(r"always\(\)|!cancelled\(\)", j["if"]):
        return None
    for n in j["needs"]:
        if n not in jobs:
            return "job %s needs unknown job %s" % (jid, n)
        if n in seen:
            continue
        r = keeps_reporting(jobs, n, seen + (jid,))
        if r:
            return "job %s needs %s, which does not run on merge_group: %s" % (jid, n, r)
    return None

found = []
for f in sorted(os.listdir(wfdir)):
    if not f.endswith(".yml"):
        continue
    mg, jobs = parse(os.path.join(wfdir, f))
    for jid, j in jobs.items():
        if j["name"] == want:
            found.append((f, mg, jobs, jid))
if not found:
    print("MISSING no workflow job is named %r" % want)
    sys.exit(0)
for f, mg, jobs, jid in found:
    if not mg:
        print("FAIL %s does not listen to merge_group" % f)
        sys.exit(0)
    try:
        r = keeps_reporting(jobs, jid)
    except ValueError as e:
        print("FAIL %s: %s" % (f, e))
        sys.exit(0)
    if r:
        print("FAIL %s: %s" % (f, r))
        sys.exit(0)
print("ok %s/%s" % (found[0][0], found[0][3]))
PY
}
required_names=$(sed -E 's/#.*//; s/[[:space:]]+$//' "$repo/hack/ci-required.txt" | awk 'NF { if ($1 == "advisory") next; sub(/^(aggregator|external) /, ""); print }')
required_names=$(printf '%s\nreview\n' "$required_names" | sort -u)
mg_seen=""
while IFS= read -r name; do
	[[ -n $name ]] || continue
	mg_seen+="|$name|"
	res=$(mg_eval "$wfdir" "$name" 2>&1)
	case $res in
	ok*)
		echo "ok: required '$name' reports on merge_group ($res)"
		pass=$((pass + 1))
		;;
	MISSING*)
		# `review` is ADR 0103's job; until it is on main there is no workflow to read. Any other name must exist.
		if [[ $name == review ]]; then
			echo "ok: required 'review': no job yet (ADR 0103 not on main); this check applies once review.yml lands"
			pass=$((pass + 1))
		else
			echo "FAIL: required '$name': $res" >&2
			fail=$((fail + 1))
		fi
		;;
	*)
		echo "FAIL: required '$name': $res" >&2
		fail=$((fail + 1))
		;;
	esac
done <<<"$required_names"
for name in review claim dco gitleaks 'e2e result'; do
	if [[ $mg_seen == *"|$name|"* ]]; then
		echo "ok: '$name' is among the names checked"
		pass=$((pass + 1))
	else
		echo "FAIL: '$name' is not among the names checked" >&2
		fail=$((fail + 1))
	fi
done
# `e2e result` reads `decide`; a skipped decide on a queue build would hand the gate an empty output.
decide_if=$(awk '/^  decide:/ { j = 1; next } j && /^  [a-z]/ { j = 0 } j && /^    if:/ { sub(/^    if: */, ""); print; exit }' "$wfdir/e2e-kind.yml")
check "e2e-kind.yml: decide's if: is true on merge_group" test "$decide_if" = "github.event_name != 'pull_request' || !github.event.pull_request.draft"
check "e2e-kind.yml: result runs under always()" bash -c 'awk "/^  result:/ { j = 1; next } j && /^  [a-z]/ { j = 0 } j && /^    if: always\(\)/ { ok = 1 } END { exit !ok }" "$1"' _ "$wfdir/e2e-kind.yml"
# The checker itself: a workflow without merge_group, an upstream gated on pull_request, and an unknown form are caught.
mgtmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-mg-eval.XXXXXX") || exit 1
mkdir "$mgtmp/one" "$mgtmp/two"
printf 'name: a\non:\n  pull_request:\njobs:\n  x:\n    name: probe\n' >"$mgtmp/one/a.yml"
cat >"$mgtmp/two/b.yml" <<'YML'
name: b
on:
  merge_group:
jobs:
  up:
    name: up
    if: github.event_name == 'pull_request'
  probe:
    name: probe
    needs: up
  safe:
    name: safe
    needs: up
    if: always()
  weird:
    name: weird
    if: contains(github.ref, 'x')
YML
eval_is() { # eval_is <description> <dir> <job> <expected prefix>
	local got
	got=$(mg_eval "$2" "$3")
	if [[ $got == "$4"* ]]; then echo "ok: $1"; pass=$((pass + 1)); else echo "FAIL: $1 (got: $got)" >&2; fail=$((fail + 1)); fi
}
eval_is "eval: a workflow without merge_group is caught" "$mgtmp/one" probe "FAIL a.yml does not listen"
eval_is "eval: an upstream job gated on pull_request is caught" "$mgtmp/two" probe "FAIL b.yml: job probe needs up"
eval_is "eval: always() keeps a job reporting" "$mgtmp/two" safe "ok b.yml/safe"
eval_is "eval: an unknown if: form fails closed" "$mgtmp/two" weird "FAIL b.yml: unknown if: form"
rm -rf "$mgtmp"

echo "$pass passed, $fail failed"
exit $((fail > 0))
