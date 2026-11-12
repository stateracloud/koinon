#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for hack/ubuntu-snapshot-bump.sh (issue 33). No network: the script reads a snapshot
# tree built here from the stanzas under hack/testdata/ubuntu-snapshot (Packages.xz plus an InRelease
# that lists their SHA256), and a USN checkout made of three small records of the OSV shape.
# Covers: the 3-day snapshot picker (month and year rollover, leap day), an explicit too-young
# snapshot, the newest version across pockets (epochs, `~`), amd64 and arm64 disagreeing, a Packages
# hash that does not match InRelease, no change, --write touching exactly the expected lines, the
# USN report (found, none found, lookup failed, skipped), and gpgv on a signed InRelease.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script=$here/ubuntu-snapshot-bump.sh
data=$here/testdata/ubuntu-snapshot
fail=0
ok() { echo "ok:   $1"; }
bad() { echo "FAIL: $1"; fail=1; }
eq() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }
has() { if grep -qF -- "$3" <<<"$2"; then ok "$1"; else bad "$1 (missing '$3' in: $2)"; fi; }
lacks() { if grep -qF -- "$3" <<<"$2"; then bad "$1 (unexpected '$3' in: $2)"; else ok "$1"; fi; }

for t in dpkg xz gzip sha256sum jq; do
	command -v "$t" >/dev/null || { echo "SKIP: $t is not installed (the script needs dpkg and xz)"; exit 0; }
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ubuntu-snapshot-bump-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

TODAY=2026-11-11
SNAP=20261108T000000Z

# run <args...>: the script against the fixture tree, output in $out, exit status in $rc.
out='' rc=0
run() { out=$("$script" "$@" 2>&1) && rc=0 || rc=$?; }

# --- the picker ------------------------------------------------------------------------------------
pick() { "$script" --print-snapshot --today "$1"; }
eq "picks today-3d" "$(pick 2026-11-11)" 20261108T000000Z
eq "month rollover" "$(pick 2026-03-02)" 20260227T000000Z
eq "year rollover" "$(pick 2026-01-02)" 20251230T000000Z
eq "year rollover on 1 Jan" "$(pick 2026-01-01)" 20251229T000000Z
eq "leap day" "$(pick 2024-03-02)" 20240228T000000Z
eq "after the leap day" "$(pick 2024-03-03)" 20240229T000000Z
eq "30-day month" "$(pick 2026-05-01)" 20260428T000000Z

run --print-snapshot --today $TODAY --snapshot 20261108T000000Z
eq "an explicit snapshot exactly 3 days old is allowed" "$rc $out" "0 20261108T000000Z"
run --print-snapshot --today $TODAY --snapshot 20261108T000001Z
eq "one second too young is refused" "$rc" 1
has "refusal names ADR 0079" "$out" "younger than 3 days (ADR 0079)"
run --print-snapshot --today $TODAY --snapshot 20261111T000000Z
eq "a snapshot of today is refused" "$rc" 1
run --print-snapshot --today $TODAY --snapshot 20301110T000000Z
eq "a future snapshot is refused" "$rc" 1
run --print-snapshot --today $TODAY --snapshot yesterday
eq "a malformed snapshot is refused" "$rc" 1
run --print-snapshot --today 2026-13-40
eq "a malformed --today is refused" "$rc" 1

# --- a snapshot tree built from the fixture -----------------------------------------------------------
suites=(noble noble-updates noble-security)
comps=(main universe)

# mkrepo DIR SNAPSHOT SRC: DIR/<snapshot>/dists/<suite>/{InRelease,<comp>/binary-<arch>/Packages.xz},
# built from SRC/<suite>.<comp>.Packages.
mkrepo() {
	local dir=$1 snap=$2 src=$3 suite comp arch f rel h size
	for suite in "${suites[@]}"; do
		mkdir -p "$dir/$snap/dists/$suite"
		{
			echo "-----BEGIN PGP SIGNED MESSAGE-----"
			echo "Hash: SHA512"
			echo
			echo "Origin: Ubuntu"
			echo "Suite: $suite"
			echo "SHA256:"
		} >"$dir/$snap/dists/$suite/InRelease"
		for comp in "${comps[@]}"; do
			for arch in amd64 arm64; do
				rel="$comp/binary-$arch/Packages"
				mkdir -p "$dir/$snap/dists/$suite/$comp/binary-$arch"
				f=$src/$suite.$comp.Packages
				if [[ -f $f ]]; then xz -c "$f" >"$dir/$snap/dists/$suite/$rel.xz"; else : | xz -c >"$dir/$snap/dists/$suite/$rel.xz"; fi
				h=$(sha256sum "$dir/$snap/dists/$suite/$rel.xz")
				size=$(wc -c <"$dir/$snap/dists/$suite/$rel.xz")
				printf ' %s %s %s\n' "${h%% *}" "$size" "$rel.xz" >>"$dir/$snap/dists/$suite/InRelease"
			done
		done
		echo "-----BEGIN PGP SIGNATURE-----" >>"$dir/$snap/dists/$suite/InRelease"
	done
}
# reindex DIR SUITE: refresh one InRelease's hash lines after a test changed an index.
reindex() {
	local dir=$1 suite=$2 comp arch rel h size
	local ir=$dir/$SNAP/dists/$suite/InRelease
	sed -i.bak '/^SHA256:/,$d' "$ir"
	echo "SHA256:" >>"$ir"
	for comp in "${comps[@]}"; do
		for arch in amd64 arm64; do
			rel="$comp/binary-$arch/Packages.xz"
			h=$(sha256sum "$dir/$SNAP/dists/$suite/$rel")
			size=$(wc -c <"$dir/$SNAP/dists/$suite/$rel")
			printf ' %s %s %s\n' "${h%% *}" "$size" "$rel" >>"$ir"
		done
	done
}

CUR=20261027T000000Z # the ARG of the fixture Dockerfile: the current snapshot
repo=$tmp/repo
mkrepo "$repo" "$SNAP" "$data/repo"
mkrepo "$repo" "$CUR" "$data/repo-cur"
base=(--base-url "$repo" --no-gpg --today "$TODAY")

# fresh_files DIR: copies of the fixture Dockerfile and README to edit.
fresh_files() { mkdir -p "$1" && cp "$data/Dockerfile" "$data/README.md" "$1/"; }

# --- check: newest version across pockets, epochs, ~ -----------------------------------------------------
fresh_files "$tmp/a"
run --check "${base[@]}" --dockerfile "$tmp/a/Dockerfile" --readme "$tmp/a/README.md"
eq "check exits 10 when pins change" "$rc" 10
has "git takes the epoch version from noble-security, not 2.99.0-1" "$out" "1:2.43.0-1ubuntu7.5"
lacks "git does not take the epoch-less 2.99.0-1" "$out" "2.99.0-1 "
has "ca-certificates takes .2, not .2~rc1" "$out" "20260816ubuntu0.24.04.2  "
has "tree comes from universe" "$out" "2.1.1-2ubuntu3.24.04.3"
has "an unchanged pin is listed" "$out" "python-is-python3"
eq "check wrote nothing" "$(cmp "$tmp/a/Dockerfile" "$data/Dockerfile" && echo same)" same
has "the snapshot is the picked one" "$out" "20261027T000000Z -> $SNAP"
has "no-gpg is announced" "$out" "InRelease signatures are NOT verified"

# --- write: exactly the expected lines -------------------------------------------------------------------
run --write "${base[@]}" --dockerfile "$tmp/a/Dockerfile" --readme "$tmp/a/README.md"
eq "write exits 10 (it changed files)" "$rc" 10
eq "Dockerfile is as expected" "$(diff "$tmp/a/Dockerfile" "$data/expected/Dockerfile" && echo same)" same
eq "README is as expected" "$(diff "$tmp/a/README.md" "$data/expected/README.md" && echo same)" same
eq "the diff is 3 pins + the ARG" "$(diff "$data/Dockerfile" "$tmp/a/Dockerfile" | grep -c '^>')" 4

# --- no change: the written files are a fixed point ---------------------------------------------------------
run --check "${base[@]}" --dockerfile "$tmp/a/Dockerfile" --readme "$tmp/a/README.md"
eq "no change exits 0" "$rc" 0
has "no change says so" "$out" "no pin changes"
run --write "${base[@]}" --dockerfile "$tmp/a/Dockerfile" --readme "$tmp/a/README.md"
eq "write with no change exits 0" "$rc" 0
eq "and leaves the Dockerfile alone" "$(diff "$tmp/a/Dockerfile" "$data/expected/Dockerfile" && echo same)" same

# A newer snapshot in the Dockerfile than the picked one: nothing to do, nothing fetched.
fresh_files "$tmp/n"
sed -i.bak 's/^ARG UBUNTU_SNAPSHOT=.*/ARG UBUNTU_SNAPSHOT=20261109T000000Z/' "$tmp/n/Dockerfile"
run --check --base-url "$tmp/missing" --no-gpg --today $TODAY --dockerfile "$tmp/n/Dockerfile" --readme "$tmp/n/README.md"
eq "a newer pinned snapshot is left alone" "$rc" 0

# --- arm64 and amd64 disagree ------------------------------------------------------------------------------
cp -r "$repo" "$tmp/repo-diff"
{
	cat "$data/repo/noble-security.main.Packages"
	printf '\nPackage: git\nVersion: 1:2.43.0-1ubuntu7.6\nArchitecture: arm64\n'
} | xz -c >"$tmp/repo-diff/$SNAP/dists/noble-security/main/binary-arm64/Packages.xz"
reindex "$tmp/repo-diff" noble-security
fresh_files "$tmp/d"
run --check --base-url "$tmp/repo-diff" --no-gpg --today $TODAY --dockerfile "$tmp/d/Dockerfile" --readme "$tmp/d/README.md"
eq "an arch disagreement fails" "$rc" 1
has "and names the package and both versions" "$out" "git resolves to 1:2.43.0-1ubuntu7.5 on amd64 and 1:2.43.0-1ubuntu7.6 on arm64"
eq "and writes nothing" "$(cmp "$tmp/d/Dockerfile" "$data/Dockerfile" && echo same)" same
run --write --base-url "$tmp/repo-diff" --no-gpg --today $TODAY --dockerfile "$tmp/d/Dockerfile" --readme "$tmp/d/README.md"
eq "--write fails too and writes nothing" "$rc $(cmp "$tmp/d/Dockerfile" "$data/Dockerfile" && echo same)" "1 same"

# --- a Packages index that does not match InRelease -------------------------------------------------------------
cp -r "$repo" "$tmp/repo-hash"
printf '\nPackage: git\nVersion: 1:2.43.0-1ubuntu7.9\nArchitecture: amd64\n' | xz -c >"$tmp/repo-hash/$SNAP/dists/noble-security/main/binary-amd64/Packages.xz"
run --check --base-url "$tmp/repo-hash" --no-gpg --today $TODAY --dockerfile "$tmp/d/Dockerfile" --readme "$tmp/d/README.md"
eq "a hash mismatch fails" "$rc" 1
has "and says which index" "$out" "main/binary-amd64/Packages.xz SHA256"

# --- an index InRelease lists that the snapshot does not serve ---------------------------------------------------
cp -r "$repo" "$tmp/repo-gone"
rm "$tmp/repo-gone/$SNAP/dists/noble-updates/universe/binary-arm64/Packages.xz"
fresh_files "$tmp/x"
run --check --base-url "$tmp/repo-gone" --no-gpg --today $TODAY --dockerfile "$tmp/x/Dockerfile" --readme "$tmp/x/README.md"
eq "a listed index that cannot be fetched fails" "$rc" 1
has "and says which" "$out" "noble-updates: cannot fetch universe/binary-arm64/Packages.xz"

# --- a pin newer than the snapshot is never a downgrade ----------------------------------------------------------
fresh_files "$tmp/g"
sed -i.bak 's/git=1:2.43.0-1ubuntu7.4/git=1:2.43.0-1ubuntu7.8/' "$tmp/g/Dockerfile"
run --check "${base[@]}" --dockerfile "$tmp/g/Dockerfile" --readme "$tmp/g/README.md"
eq "a pin newer than the snapshot fails" "$rc" 1
has "and says so" "$out" "older than the pin"

# A package the snapshot lacks.
fresh_files "$tmp/m"
sed -i.bak 's/^      zstd=/      nosuchpkg=1.0 \\\n      zstd=/' "$tmp/m/Dockerfile"
run --check "${base[@]}" --dockerfile "$tmp/m/Dockerfile" --readme "$tmp/m/README.md"
eq "a pin missing from the snapshot fails" "$rc" 1

# --- dependencies: the pins stay, a dependency of theirs gets newer ----------------------------------------------------
# The check reports the dependency closure drift between the current and the new snapshot.
fresh_files "$tmp/a2"
run --check "${base[@]}" --dockerfile "$tmp/a2/Dockerfile" --readme "$tmp/a2/README.md"
has "a newer dependency is listed" "$out" "libdep1"
has "with both versions" "$out" "1.0-1ubuntu0.2"
lacks "a virtual dependency resolved to its provider that did not change is not listed" "$out" "libprov "
lacks "an unchanged dependency is not listed" "$out" "libc6 "

# Only a dependency changed: the Dockerfile's pins already are the new candidates, the current snapshot
# (20261020) has the same packages except an older libdep1. --check must exit 10 and --write move the ARG.
DEP=20261020T000000Z
cp -r "$data/repo" "$tmp/src-dep"
awk 'BEGIN{RS="";ORS="\n\n"} !/^Package: libdep1\nVersion: 1.0-1ubuntu0.2/' "$tmp/src-dep/noble-updates.main.Packages" >"$tmp/src-dep/x" && mv "$tmp/src-dep/x" "$tmp/src-dep/noble-updates.main.Packages"
mkrepo "$repo" "$DEP" "$tmp/src-dep"
mkdir -p "$tmp/o"
cp "$data/expected/Dockerfile" "$data/expected/README.md" "$tmp/o/"
sed -i.bak "s/20261108T000000Z/$DEP/" "$tmp/o/Dockerfile" "$tmp/o/README.md"
cp "$tmp/o/Dockerfile" "$tmp/o/Dockerfile.orig"
run --check "${base[@]}" --dockerfile "$tmp/o/Dockerfile" --readme "$tmp/o/README.md"
eq "only a dependency is newer: --check exits 10" "$rc" 10
has "and lists the dependency" "$out" "libdep1"
has "and says that no pin changes" "$out" "0 pin(s) and 1 dependenc"
eq "--check wrote nothing" "$(cmp "$tmp/o/Dockerfile" "$tmp/o/Dockerfile.orig" && echo same)" same
run --write "${base[@]}" --dockerfile "$tmp/o/Dockerfile" --readme "$tmp/o/README.md" --usn-dir "$data/usn" --report "$tmp/o/report.md"
eq "only a dependency is newer: --write exits 10" "$rc" 10
eq "and moves the ARG, nothing else" "$(diff "$tmp/o/Dockerfile" "$data/expected/Dockerfile" && echo same)" same
eq "and the README's snapshot ID" "$(diff "$tmp/o/README.md" "$data/expected/README.md" && echo same)" same
oreport=$(<"$tmp/o/report.md")
has "report: the dependency table" "$oreport" "| \`libdep1\` | \`1.0-1\` | \`1.0-1ubuntu0.2\` |"
has "report: the USN of the dependency's binary" "$oreport" "USN-9004-1"

# Nothing changed at all: the current snapshot has the same indexes as the new one.
SAME=20261021T000000Z
mkrepo "$repo" "$SAME" "$data/repo"
cp "$tmp/o/Dockerfile.orig" "$tmp/o/Dockerfile"
sed -i.bak "s/^ARG UBUNTU_SNAPSHOT=.*/ARG UBUNTU_SNAPSHOT=$SAME/" "$tmp/o/Dockerfile"
run --check "${base[@]}" --dockerfile "$tmp/o/Dockerfile" --readme "$tmp/o/README.md"
eq "same indexes: --check exits 0" "$rc" 0
has "and says nothing is newer" "$out" "no pin changes and no newer dependency"

# A current snapshot that cannot be fetched fails the run (the comparison needs it).
cp "$tmp/o/Dockerfile.orig" "$tmp/o/Dockerfile"
sed -i.bak "s/^ARG UBUNTU_SNAPSHOT=.*/ARG UBUNTU_SNAPSHOT=20200101T000000Z/" "$tmp/o/Dockerfile"
run --check "${base[@]}" --dockerfile "$tmp/o/Dockerfile" --readme "$tmp/o/README.md"
eq "an unfetchable current snapshot fails" "$rc" 1
has "and says why" "$out" "the current snapshot 20200101T000000Z is needed"

# --- USN annotation ---------------------------------------------------------------------------------------------------
fresh_files "$tmp/u"
run --check "${base[@]}" --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md" --usn-dir "$data/usn" --usn-commit deadbeef --report "$tmp/u/report.md"
eq "check with USNs exits 10" "$rc" 10
has "USN lookup ok" "$out" "USN lookup: ok"
report=$(<"$tmp/u/report.md")
has "report: the USN in range" "$report" "USN-9002-1"
has "report: its CVE and priority" "$report" "priority high, CVE-2099-0001"
lacks "report: a USN at or below the old pin is not listed" "$report" "USN-9003-1"
has "report: the ca-certificates USN (noble only)" "$report" "USN-9001-1"
lacks "report: the 22.04 record of that USN is not used" "$report" "22.04"
has "report: tree has no USN, said plainly" "$report" "| \`tree\` | \`2.1.1-2ubuntu3.24.04.2\` | \`2.1.1-2ubuntu3.24.04.3\` | none found"
has "report: the USN repo commit" "$report" "at \`deadbeef\`"
has "report: both snapshot IDs" "$report" "\`20261027T000000Z\` -> \`$SNAP\`"
has "report: high priority is flagged for a human" "$report" "security fast path of ADR 0079 is a human decision"

run --check "${base[@]}" --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md" --usn-dir "$tmp/nowhere" --report "$tmp/u/report2.md"
report=$(<"$tmp/u/report2.md")
has "a USN lookup that cannot run is FAILED, not 'none'" "$report" "lookup FAILED"
lacks "and never says 'none found'" "$report" "none found"
mkdir -p "$tmp/emptyusn/osv/usn"
run --check "${base[@]}" --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md" --usn-dir "$tmp/emptyusn" --report "$tmp/u/report3.md"
has "an empty USN checkout is FAILED" "$(<"$tmp/u/report3.md")" "FAILED: no USN-*.json"
mkdir -p "$tmp/badusn/osv/usn"
echo '{not json' >"$tmp/badusn/osv/usn/USN-1-1.json"
run --check "${base[@]}" --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md" --usn-dir "$tmp/badusn" --report "$tmp/u/report4.md"
has "unparsable USN records are FAILED" "$(<"$tmp/u/report4.md")" "FAILED: jq"
run --check "${base[@]}" --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md" --report "$tmp/u/report5.md"
has "no --usn-dir is reported as skipped" "$(<"$tmp/u/report5.md")" "skipped"
eq "a failed USN lookup does not change the exit code" "$rc" 10
# The banner counts only USNs in (old pin, new]: a high one at or below the old pin does not raise it.
mkdir -p "$tmp/lowusn/osv/usn"
cp "$data/usn/osv/usn/USN-9001-1.json" "$data/usn/osv/usn/USN-9004-1.json" "$tmp/lowusn/osv/usn/"
sed 's/"score":"low"/"score":"high"/' "$data/usn/osv/usn/USN-9003-1.json" >"$tmp/lowusn/osv/usn/USN-9003-1.json"
run --check "${base[@]}" --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md" --usn-dir "$tmp/lowusn" --report "$tmp/u/report6.md"
lacks "a high USN outside the range does not raise the banner" "$(<"$tmp/u/report6.md")" "security fast path"

# --- signatures: gpgv on a real signed InRelease ----------------------------------------------------------------------
if command -v gpg >/dev/null && command -v gpgv >/dev/null; then
	gh=$tmp/gnupg
	mkdir -m 700 "$gh"
	if GNUPGHOME=$gh gpg --batch --quiet --pinentry-mode loopback --passphrase '' --quick-generate-key 'koinon test <test@example.invalid>' ed25519 sign never >/dev/null 2>&1; then
		GNUPGHOME=$gh gpg --batch --quiet --export >"$tmp/test-keyring.gpg"
		fpr=$(GNUPGHOME=$gh gpg --batch --with-colons --fingerprint 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }')
		cp -r "$repo" "$tmp/repo-signed"
		for snap in "$SNAP" "$CUR"; do for suite in "${suites[@]}"; do
			ir=$tmp/repo-signed/$snap/dists/$suite/InRelease
			# A real clearsigned file: the fixture's fake armor is replaced by gpg's.
			sed -n '/^SHA256:/,/^-----BEGIN PGP SIGNATURE/p' "$ir" | sed '$d' >"$tmp/sha.txt"
			{ echo "Origin: Ubuntu"; echo "Suite: $suite"; cat "$tmp/sha.txt"; } >"$tmp/plain.txt"
			rm -f "$ir"
			GNUPGHOME=$gh gpg --batch --quiet --pinentry-mode loopback --passphrase '' --clearsign -o "$ir" "$tmp/plain.txt"
		done; done
		fresh_files "$tmp/s"
		run --check --base-url "$tmp/repo-signed" --keyring "$tmp/test-keyring.gpg" --signer "$fpr" --today $TODAY --dockerfile "$tmp/s/Dockerfile" --readme "$tmp/s/README.md"
		eq "a signed InRelease verifies" "$rc" 10
		lacks "and no-gpg is not announced" "$out" "NOT verified"
		# A good signature from a key that is in the keyring but not pinned is refused (here: the
		# default pins, the Ubuntu archive keys, which did not sign the fixture).
		run --check --base-url "$tmp/repo-signed" --keyring "$tmp/test-keyring.gpg" --today $TODAY --dockerfile "$tmp/s/Dockerfile" --readme "$tmp/s/README.md"
		eq "a valid signature from a key that is not pinned fails" "$rc" 1
		has "and says so" "$out" "is not signed by a pinned Ubuntu archive key"
		run --check --base-url "$tmp/repo-signed" --keyring "$tmp/test-keyring.gpg" --signer 0000000000000000000000000000000000000000 --today $TODAY --dockerfile "$tmp/s/Dockerfile" --readme "$tmp/s/README.md"
		eq "an explicit pin of another key fails too" "$rc" 1
		# Text outside the clearsigned block does not change gpgv's verdict, so an unsigned SHA256
		# list put before BEGIN PGP SIGNED MESSAGE must not be read: the signed hash is the one checked.
		cp -r "$tmp/repo-signed" "$tmp/repo-prepend"
		ir=$tmp/repo-prepend/$SNAP/dists/noble-security/InRelease
		pk=$tmp/repo-prepend/$SNAP/dists/noble-security/main/binary-amd64/Packages.xz
		printf '\nPackage: git\nVersion: 1:2.43.0-1ubuntu7.9\nArchitecture: amd64\n' | xz -c >"$pk"
		h=$(sha256sum "$pk")
		{ echo "SHA256:"; printf ' %s %s %s\n' "${h%% *}" "$(wc -c <"$pk")" main/binary-amd64/Packages.xz; cat "$ir"; } >"$tmp/ir.new"
		mv "$tmp/ir.new" "$ir"
		if gpgv --keyring "$tmp/test-keyring.gpg" "$ir" >/dev/null 2>&1; then ok "gpgv accepts text before the signed block (the case this guards)"; else bad "gpgv rejected the prepended InRelease; the case no longer tests the parser"; fi
		run --check --base-url "$tmp/repo-prepend" --keyring "$tmp/test-keyring.gpg" --signer "$fpr" --today $TODAY --dockerfile "$tmp/s/Dockerfile" --readme "$tmp/s/README.md"
		eq "an unsigned SHA256 list before the signed block is ignored" "$rc" 1
		has "and the signed hash is the one checked" "$out" "main/binary-amd64/Packages.xz SHA256"
		sed -i.bak 's/^Suite: noble-security/Suite: noble-securitx/' "$tmp/repo-signed/$SNAP/dists/noble-security/InRelease"
		run --check --base-url "$tmp/repo-signed" --keyring "$tmp/test-keyring.gpg" --signer "$fpr" --today $TODAY --dockerfile "$tmp/s/Dockerfile" --readme "$tmp/s/README.md"
		eq "a tampered InRelease fails" "$rc" 1
		has "and says which" "$out" "InRelease of noble-security does not verify"
		# A correctly signed InRelease of another suite in the place of this one is refused.
		cp -r "$tmp/repo-signed" "$tmp/repo-wrongsuite"
		cp "$tmp/repo-signed/$SNAP/dists/noble/InRelease" "$tmp/repo-wrongsuite/$SNAP/dists/noble-updates/InRelease"
		run --check --base-url "$tmp/repo-wrongsuite" --keyring "$tmp/test-keyring.gpg" --signer "$fpr" --today $TODAY --dockerfile "$tmp/s/Dockerfile" --readme "$tmp/s/README.md"
		eq "a signed InRelease of another suite fails" "$rc" 1
		has "and says which suite" "$out" "InRelease of noble-updates says Suite 'noble', not 'noble-updates'"
	else
		echo "SKIP: gpg could not create a test key; signature cases not run"
	fi
else
	echo "SKIP: gpg and gpgv are not installed; signature cases not run"
fi
run --check --base-url "$repo" --keyring "$tmp/no-such-keyring.gpg" --today $TODAY --dockerfile "$tmp/u/Dockerfile" --readme "$tmp/u/README.md"
eq "without a keyring and without --no-gpg it refuses" "$rc" 1

[[ $fail -eq 0 ]] && echo "ubuntu-snapshot-bump tests passed"
exit "$fail"
