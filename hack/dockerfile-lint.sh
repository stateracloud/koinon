#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Lints every Dockerfile in the tree (#175).
#
#   hack/dockerfile-lint.sh [FILE...]    default: every tracked Dockerfile, *.Dockerfile and Dockerfile.*
#
# Two passes:
#   1. hadolint (pinned in hack/versions.env, config .hadolint.yaml): the generic Dockerfile and
#      embedded-shell rules.
#   2. A small awk pass for the two Koinon rules hadolint has no rule for:
#        digest  every base image (FROM) is pinned by digest. The image may come from an ARG: its
#                default must hold @sha256:<64 hex> (an ARG without a default is supplied by the
#                caller and is not checked). A previous stage and `scratch` need no digest.
#        secret  no ENV or ARG is named like a secret (TOKEN, SECRET, PASSWORD, API_KEY, ...): the value
#                lands in the image history. Use a BuildKit secret mount (see hack/e2e-toolchain/Dockerfile).
#                Names ending _FILE or _PATH (a path, not the value) are fine. Values are gitleaks' job.
#      Apt retries are hack/apt-retries-test.sh (#149), not repeated here.
# An exception is a comment line directly above the instruction (comment lines may follow each other):
#   # dockerfile-lint: allow=digest <reason>      (or allow=secret); the reason is required.
# hack/testdata/ is left out: its Dockerfiles are fixtures and golden output of script tests.
#
# Exit 0 clean, 1 findings (the awk pass runs first and always), 2 usage, 3 hadolint is not installed
# (the awk pass still ran; CI sets KOINON_REQUIRE=hadolint, which turns the 3 into a 1, as for Docker
# in hack/go-test.sh). HADOLINT=<path> picks another binary (the test does).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

# check FILE: one line per violation of the digest and secret rules.
check() {
	awk '
		function flush(   rest, w, n, img, name, k, v, a, i, ref) {
			if (instr == "") return
			rest = instr
			sub(/^[A-Za-z]+[ \t]+/, "", rest)
			kw = toupper(substr(instr, 1, index(instr, " ") ? index(instr, " ") - 1 : length(instr)))
			if (kw == "ARG") {
				n = split(rest, w, /[ \t]+/)
				for (i = 1; i <= n; i++) {
					k = w[i]; v = ""
					if (index(k, "=")) { v = substr(k, index(k, "=") + 1); k = substr(k, 1, index(k, "=") - 1) }
					argdef[k] = (index(w[i], "=") ? v : "\001")
					secret_check(k)
				}
			} else if (kw == "ENV") {
				n = split(rest, w, /[ \t]+/)
				if (w[1] !~ /=/) secret_check(w[1])
				else for (i = 1; i <= n; i++) if (w[i] ~ /^[A-Za-z_][A-Za-z0-9_]*=/) secret_check(substr(w[i], 1, index(w[i], "=") - 1))
			} else if (kw == "FROM") {
				n = split(rest, w, /[ \t]+/)
				i = 1
				while (i <= n && w[i] ~ /^--/) i++
				img = w[i]; name = ""
				if (toupper(w[i + 1]) == "AS") name = tolower(w[i + 2])
				digest_check(img)
				if (name != "") stages[name] = 1
			}
			instr = ""
		}
		# @sha256: followed by exactly 64 hex digits (no interval expressions: mawk lacks them)
		function is_digest(ref,   p, h) {
			p = index(ref, "@sha256:")
			if (p == 0) return 0
			h = substr(ref, p + 8)
			sub(/[^0-9a-f].*$/, "", h)
			return length(h) == 64
		}
		function allowed(rule) { return (allow ~ ("(^| )" rule "( |$)")) }
		function secret_check(k) {
			if (toupper(k) ~ /(TOKEN|SECRET|PASSW(OR)?D|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|CREDENTIAL)/ && toupper(k) !~ /_(FILE|PATH)$/ && !allowed("secret"))
				printf "%s:%d: secret: %s is named like a secret; its value would be in the image history (use a BuildKit secret mount, or # dockerfile-lint: allow=secret <reason>)\n", FILENAME, start, k
		}
		function digest_check(img,   ref, d) {
			if (tolower(img) == "scratch" || (tolower(img) in stages)) return
			ref = img
			# an image held in an ARG: judge the ARG default; no default means the caller supplies it
			if (match(ref, /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/)) {
				d = substr(ref, 3, length(ref) - 3)
				if (!(d in argdef) || argdef[d] == "\001") return
				ref = argdef[d]
			}
			# a stage chosen by ARG (fetch-${TARGETARCH}) is a stage name with a variable suffix
			if (ref ~ /\$\{/) {
				d = ref; sub(/\$\{.*$/, "", d)
				for (k in stages) if (d != "" && index(k, d) == 1) return
			}
			if (is_digest(ref)) return
			if (ref ~ /@\$\{[A-Za-z_][A-Za-z0-9_]*\}/) {
				d = ref; sub(/^.*@\$\{/, "", d); sub(/\}.*$/, "", d)
				if (!(d in argdef) || argdef[d] == "\001" || argdef[d] ~ /^sha256:[0-9a-f]+$/ && length(argdef[d]) == 71) return
			}
			if (!allowed("digest"))
				printf "%s:%d: digest: base image %s is not pinned by @sha256:<digest> (or # dockerfile-lint: allow=digest <reason>)\n", FILENAME, start, img
		}
		/^[ \t]*#/ {
			if (instr == "") {
				if (match($0, /dockerfile-lint:[ \t]*allow=[a-z]+[ \t]+[^ \t]/)) {
					a = $0; sub(/^.*dockerfile-lint:[ \t]*allow=/, "", a); sub(/[ \t].*$/, "", a)
					pending = pending " " a
				}
			}
			next
		}
		/^[ \t]*$/ { if (instr == "") pending = ""; next }
		{
			if (instr == "") { start = FNR; allow = pending; pending = "" }
			line = $0
			cont = (line ~ /\\[ \t]*$/)
			sub(/\\[ \t]*$/, "", line)
			sub(/^[ \t]+/, "", line)
			instr = (instr == "" ? "" : instr " ") line
			if (!cont) flush()
		}
		END { flush() }
	' "$1"
}

if [ $# -gt 0 ]; then
	files=("$@")
else
	cd "$root"
	mapfile -t files < <(git ls-files -z -- '*Dockerfile' '*Dockerfile.*' '*.Dockerfile' ':!:hack/testdata/*' | tr '\0' '\n')
	[ "${#files[@]}" -gt 0 ] || { echo "dockerfile-lint: found no Dockerfiles" >&2; exit 2; }
fi

bad=0
for f in "${files[@]}"; do
	out=$(check "$f")
	if [ -n "$out" ]; then echo "$out" >&2; bad=1; fi
done

hl=${HADOLINT:-hadolint}
if command -v "$hl" >/dev/null 2>&1; then
	# shellcheck source=/dev/null
	. "$root/hack/versions.env"
	have=$("$hl" --version 2>/dev/null | sed -n 's/.*[ v]\([0-9][0-9.]*\).*/\1/p' | head -1)
	[ "$have" = "$HADOLINT_VERSION" ] || echo "dockerfile-lint: warn: CI pins hadolint $HADOLINT_VERSION (hack/versions.env), this is ${have:-unknown}; findings can differ" >&2
	"$hl" --config "$root/.hadolint.yaml" "${files[@]}" || bad=1
	[ "$bad" -eq 0 ] || exit 1
	echo "dockerfile-lint: ok (${#files[@]} Dockerfiles)"
	exit 0
fi

[ "$bad" -eq 0 ] || exit 1
case ",${KOINON_REQUIRE:-}," in
*,hadolint,*) echo "dockerfile-lint: hadolint is required (KOINON_REQUIRE) and not installed" >&2; exit 1 ;;
esac
echo "dockerfile-lint: INCOMPLETE (missing hadolint): the digest and secret rules passed on ${#files[@]} Dockerfiles; CI pins hadolint $(sed -n 's/^HADOLINT_VERSION=//p' "$root/hack/versions.env") (hack/versions.env)" >&2
exit 3
