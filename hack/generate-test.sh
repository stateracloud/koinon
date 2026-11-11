#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/generate.sh (#349 / old #464): when buf fails after its clean step wiped a
# tracked generated file, the script restores the tree. No network, no real buf.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.." || exit 2
script=$PWD/hack/generate.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/generate-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
export TMPDIR=$tmp

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

repo=$tmp/repo
mkdir -p "$repo/web/src/gen/google/rpc" "$repo/internal/gen" "$repo/hack" \
	"$repo/internal/db/store" "$repo/internal/audit/internal/auditstore" "$repo/internal/adminapi" \
	"$repo/api/admin/v1" "$tmp/bin"
echo 'tracked' >"$repo/web/src/gen/google/rpc/error_details_pb.ts"
echo 'go-gen' >"$repo/internal/gen/keep.go"
echo 'store' >"$repo/internal/db/store/models.go"
echo 'audit' >"$repo/internal/audit/internal/auditstore/models.go"
echo 'api' >"$repo/internal/adminapi/api.gen.go"
: >"$repo/api/admin/v1/openapi.yaml"
: >"$repo/api/admin/v1/oapi-codegen.yaml"
git -C "$repo" init -q -b main
git -C "$repo" config user.name t
git -C "$repo" config user.email t@example.invalid
git -C "$repo" add -A
git -C "$repo" commit -q -m init

# Stub buf: first plain generate deletes the tracked googleapis file (like clean:true); the
# googleapis template then fails as if buf.build were unreachable.
cat >"$tmp/bin/buf" <<'S'
#!/usr/bin/env bash
echo "buf $*" >>"${BUF_LOG:-/dev/null}"
# Parse: `buf generate` or `buf generate --template X`
if [[ ${1:-} != generate ]]; then exit 0; fi
shift
template=""
while [[ $# -gt 0 ]]; do
	case $1 in
	--template) template=$2; shift 2 ;;
	*) shift ;;
	esac
done
case $template in
"")
	rm -f web/src/gen/google/rpc/error_details_pb.ts
	mkdir -p web/src/gen internal/gen
	echo 'regen' >internal/gen/keep.go
	exit 0
	;;
buf.gen.googleapis.yaml)
	echo "Failure: the server hosted at that remote is unavailable." >&2
	exit 1
	;;
*)
	exit 0
	;;
esac
S
cat >"$tmp/bin/go" <<'S'
#!/usr/bin/env bash
exit 0
S
chmod +x "$tmp/bin/buf" "$tmp/bin/go"

rc=0
BUF_LOG=$tmp/buf.log PATH="$tmp/bin:$PATH" GENERATE_ROOT="$repo" bash "$script" >"$tmp/out" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then ok "generate exits non-zero on buf failure (rc=$rc)"; else bad "generate exited 0 on buf failure"; fi
if [[ -f $repo/web/src/gen/google/rpc/error_details_pb.ts ]] && grep -qx tracked "$repo/web/src/gen/google/rpc/error_details_pb.ts"; then
	ok "tracked error_details_pb.ts restored"
else
	bad "error_details_pb.ts not restored"
	git -C "$repo" status --porcelain | sed 's/^/    /' || true
fi
if [[ -f $repo/internal/gen/keep.go ]] && grep -qx go-gen "$repo/internal/gen/keep.go"; then
	ok "internal/gen restored"
else
	bad "internal/gen not restored to pre-generate state"
fi
if git -C "$repo" status --porcelain | grep -q .; then
	bad "git status not clean after failed generate"
	git -C "$repo" status --porcelain | sed 's/^/    /'
else
	ok "git status clean after failed generate"
fi
grep -q 'restoring generated outputs' "$tmp/out" || bad "restore message missing"

# Success path: every buf step and go tool succeed; tree may change and is kept.
cat >"$tmp/bin/buf" <<'S'
#!/usr/bin/env bash
mkdir -p web/src/gen/google/rpc internal/gen
echo 'new' >web/src/gen/google/rpc/error_details_pb.ts
echo 'new-go' >internal/gen/keep.go
exit 0
S
chmod +x "$tmp/bin/buf"
rc=0
PATH="$tmp/bin:$PATH" GENERATE_ROOT="$repo" bash "$script" >"$tmp/ok.out" 2>&1 || rc=$?
if [[ $rc -eq 0 ]]; then ok "generate succeeds when buf works"; else bad "generate failed unexpectedly"; sed 's/^/    /' "$tmp/ok.out"; fi
if grep -qx new "$repo/web/src/gen/google/rpc/error_details_pb.ts"; then
	ok "successful generate keeps new output"
else
	bad "successful generate did not keep new output"
fi

[[ $fail -eq 0 ]] && echo "generate-test: ok"
exit "$fail"
