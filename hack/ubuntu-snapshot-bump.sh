#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Ubuntu snapshot bump (issue 33): moves UBUNTU_SNAPSHOT and the exact apt pins of the OpenCode flavor
# image (flavors/opencode/Dockerfile) to the newest versions of a snapshot.ubuntu.com snapshot.
#
# What it does
#   1. Picks the snapshot ID: midnight UTC of (today - 3 days), YYYYMMDDT000000Z. A snapshot ID
#      resolves to the latest publication at or before it, so every package in it was published at
#      least 3 days ago: ADR 0079's cooldown holds by construction. An explicit --snapshot younger
#      than 3 days is refused. The security fast path of ADR 0079 stays a human act (a hand-written
#      PR with the dependency-age waiver); this script never takes it.
#   2. For each of noble, noble-updates and noble-security (the Suites line of the Dockerfile's
#      deb822 source) x each Component x amd64 and arm64 it reads InRelease and the Packages index.
#   3. For every package the Dockerfile pins (`name=version \` lines of the apt install), takes the
#      highest version across all suites and components with `dpkg --compare-versions` (apt installs
#      the highest candidate; epochs and `~` sort as dpkg sorts them). The two architectures must
#      agree, or the run fails: a pin is one string for both image platforms.
#      The pins are not the whole security surface: apt takes every dependency that is not pinned
#      (python3.12 behind python3, curl and expat behind git, ...) from whatever snapshot the ARG names.
#      So the script also resolves the dependency closure of the pins (Depends and Pre-Depends, first
#      alternative, highest version, from the same indexes) in the current and in the new snapshot, and
#      a dependency that is newer in the new one counts as a change on its own: the ARG moves even when
#      no pin does. The closure over-approximates (what the base image has is included).
#   4. --check prints pinned vs candidate; --write rewrites UBUNTU_SNAPSHOT and the changed pins in
#      the Dockerfile and the snapshot ID in flavors/opencode/README.md, and nothing else.
#   5. Best effort: for each changed package it lists the Ubuntu Security Notices that fix a binary
#      version in (pinned, candidate] from canonical/ubuntu-security-notices (osv/usn/*.json, the
#      Ubuntu:24.04:LTS records, matched on ecosystem_specific.binaries). A package in noble-security
#      does not imply a USN (a security upload can ship without a notice), so "no USN found" is a
#      finding, not an error; a lookup that could not run is reported as FAILED, never as "none".
#
# What is verified
#   - InRelease is checked with gpgv against the Ubuntu archive keyring (--keyring, default
#     /usr/share/keyrings/ubuntu-archive-keyring.gpg). Without a keyring the run fails unless
#     --no-gpg is given (tests and fixtures only).
#   - Every Packages index is checked against the SHA256 the verified InRelease lists for it.
#   - Not verified: the .deb files themselves (apt does that at build time against the same keys),
#     and a USN's claim of which version fixes what.
#
# Usage
#   hack/ubuntu-snapshot-bump.sh --check [options]     exit 0: nothing changes, 10: a pin or a dependency would change
#   hack/ubuntu-snapshot-bump.sh --write [options]     edit the files; exit 10 when it changed them, 0 when not
#   hack/ubuntu-snapshot-bump.sh --print-snapshot [--snapshot ID] [--today D]   print the ID that would be used
# Options
#   --snapshot ID        use this snapshot instead of today-3d (refused when younger than 3 days)
#   --today YYYY-MM-DD   treat this day (00:00 UTC) as now (tests)
#   --base-url URL|DIR   snapshot root, <base>/<ID>/dists/... (default https://snapshot.ubuntu.com/ubuntu)
#   --dockerfile FILE    default flavors/opencode/Dockerfile
#   --readme FILE        default flavors/opencode/README.md
#   --keyring FILE       archive keyring for gpgv      --no-gpg   skip the signature check (says so)
#   --usn-dir DIR        a checkout of canonical/ubuntu-security-notices (or its osv/usn directory)
#   --usn-commit SHA     the commit of that checkout, for the report
#   --report FILE        write the markdown report (table, USNs, snapshot, USN commit)
# Exit codes: 0 ok / nothing to change, 1 error, 2 usage, 10 pins change.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

MIN_AGE_DAYS=3
ARCHES=(amd64 arm64)

# --- snapshot ID arithmetic (pure bash: no GNU/BSD date differences) -------------------------------

# days_from_civil Y M D: days since 1970-01-01 (Howard Hinnant's algorithm).
days_from_civil() {
	local y=$((10#$1)) m=$((10#$2)) d=$((10#$3)) era yoe doy doe
	((m <= 2)) && y=$((y - 1))
	era=$(((y >= 0 ? y : y - 399) / 400))
	yoe=$((y - era * 400))
	doy=$(((153 * (m + (m > 2 ? -3 : 9)) + 2) / 5 + d - 1))
	doe=$((yoe * 365 + yoe / 4 - yoe / 100 + doy))
	echo $((era * 146097 + doe - 719468))
}

# civil_from_days N: "YYYY MM DD".
civil_from_days() {
	local z=$(($1 + 719468)) era doe yoe y doy mp d m
	era=$(((z >= 0 ? z : z - 146096) / 146097))
	doe=$((z - era * 146097))
	yoe=$(((doe - doe / 1460 + doe / 36524 - doe / 146096) / 365))
	y=$((yoe + era * 400))
	doy=$((doe - (365 * yoe + yoe / 4 - yoe / 100)))
	mp=$(((5 * doy + 2) / 153))
	d=$((doy - (153 * mp + 2) / 5 + 1))
	m=$((mp + (mp < 10 ? 3 : -9)))
	((m <= 2)) && y=$((y + 1))
	printf '%04d %02d %02d\n' "$y" "$m" "$d"
}

# snapshot_epoch ID: seconds since the epoch, or return 1 when ID is not YYYYMMDDTHHMMSSZ.
snapshot_epoch() {
	[[ $1 =~ ^([0-9]{4})([0-9]{2})([0-9]{2})T([0-9]{2})([0-9]{2})([0-9]{2})Z$ ]] || return 1
	echo $(($(days_from_civil "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}") * 86400 + 10#${BASH_REMATCH[4]} * 3600 + 10#${BASH_REMATCH[5]} * 60 + 10#${BASH_REMATCH[6]}))
}

# now_epoch [YYYY-MM-DD]: the override at 00:00 UTC, else the clock.
now_epoch() {
	if [[ -n ${1:-} ]]; then
		[[ $1 =~ ^([0-9]{4})-([0-9]{2})-([0-9]{2})$ ]] || { echo "bad --today '$1' (want YYYY-MM-DD)" >&2; return 1; }
		local days
		days=$(days_from_civil "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
		# A day that does not exist (2026-13-40, 2026-02-30) does not survive the round trip.
		[[ $(civil_from_days "$days") == "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]}" ]] || { echo "bad --today '$1' (no such day)" >&2; return 1; }
		echo $((days * 86400))
	else
		date -u +%s
	fi
}

# pick_snapshot NOW_EPOCH: midnight UTC of (now - 3 days).
pick_snapshot() {
	local days=$((($1 - MIN_AGE_DAYS * 86400) / 86400)) ymd
	ymd=$(civil_from_days "$days")
	read -r y m d <<<"$ymd"
	printf '%s%s%sT000000Z\n' "$y" "$m" "$d"
}

# --- Dockerfile and Packages parsing -------------------------------------------------------------

# parse_pins FILE: "name<TAB>version" for the lines of the apt install list.
parse_pins() {
	local line in=0 bs=$'\\'
	while IFS= read -r line; do
		if ((in == 0)); then
			[[ $line == *"install -y --no-install-recommends $bs" ]] && in=1
			continue
		fi
		if [[ $line =~ ^[[:space:]]+([a-z0-9][a-z0-9+.-]*)=([^[:space:]\\]+)([[:space:]]*\\)?$ ]]; then
			printf '%s\t%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
		else
			break
		fi
	done <"$1"
}

# index_records WANTED: reads a Packages index on stdin and prints, for every package named in WANTED
# (space separated) or providing one of those names, "name<TAB>version<TAB>deps<TAB>provides". deps are
# the names of the first alternative of every Depends and Pre-Depends group (version constraints and
# :any qualifiers dropped), provides the names of the Provides entries.
index_records() {
	awk -v wanted=" $1 " '
		function flush(   i, n, k, o, pl, da, pa, alt, keep) {
			if (p == "") return
			keep = index(wanted, " " p " ") > 0
			n = split(prov, pa, /, */)
			pl = ""
			for (i = 1; i <= n; i++) {
				k = pa[i]; sub(/[ (].*/, "", k)
				if (k == "") continue
				pl = pl " " k
				if (index(wanted, " " k " ")) keep = 1
			}
			if (keep) {
				n = split(dep, da, /, */)
				o = ""
				for (i = 1; i <= n; i++) {
					split(da[i], alt, / *\| */)
					k = alt[1]; sub(/[ (].*/, "", k); sub(/:.*/, "", k)
					if (k != "") o = o " " k
				}
				print p "\t" v "\t" substr(o, 2) "\t" substr(pl, 2)
			}
			p = ""; v = ""; dep = ""; prov = ""
		}
		/^Package: / { flush(); p = $2; next }
		/^Version: / { v = $2; next }
		/^(Pre-)?Depends: / { dep = dep (dep == "" ? "" : ", ") substr($0, index($0, ": ") + 2); next }
		/^Provides: / { prov = substr($0, index($0, ": ") + 2); next }
		/^$/ { flush() }
		END { flush() }'
}

dpkg_gt() { dpkg --compare-versions "$1" gt "$2"; }

# fetch REL DEST: one file below $base/$fsnap, from a URL or a directory.
fetch() {
	local rel=$1 dest=$2
	if [[ $base == http://* || $base == https://* ]]; then
		curl --silent --show-error --fail --location --retry 3 --retry-delay 2 --max-time 120 -o "$dest" "$base/$fsnap/$rel"
	else
		cp "${base#file://}/$fsnap/$rel" "$dest"
	fi
}

# release_hash INRELEASE PATH: the SHA256 InRelease lists for PATH (relative to the suite).
release_hash() {
	awk -v want="$2" '
		/^SHA256:/ { s = 1; next }
		/^[^ ]/ { s = 0 }
		s && $3 == want { print $1; exit }' "$1"
}

# append_index SNAP SUITE COMP ARCH: appends one hash-verified, decompressed Packages index to stdout.
append_index() {
	local snap=$1 suite=$2 comp=$3 arch=$4 rel ext got want f
	rel="$comp/binary-$arch/Packages"
	for ext in .xz .gz ""; do
		want=$(release_hash "$work/Release.$snap.$suite" "$rel$ext")
		[[ -n $want ]] && break
	done
	[[ -n $want ]] || { echo "$suite: InRelease lists no $rel{.xz,.gz,}" >&2; return 1; }
	f="$work/Packages.$snap.$suite.$comp.$arch"
	fetch "dists/$suite/$rel$ext" "$f$ext" || { echo "$suite: cannot fetch $rel$ext" >&2; return 1; }
	got=$(sha256sum "$f$ext")
	got=${got%% *}
	[[ $got == "$want" ]] || { echo "$suite: $rel$ext SHA256 $got does not match InRelease ($want)" >&2; return 1; }
	case $ext in
	.xz) xz -dc "$f$ext" ;;
	.gz) gzip -dc "$f$ext" ;;
	*) cat "$f$ext" ;;
	esac
	echo
	rm -f "$f$ext"
}

# load_snapshot SNAP: verifies every InRelease and Packages index of the snapshot and leaves one
# decompressed file per architecture, $work/idx.SNAP.ARCH (all suites and components concatenated).
load_snapshot() {
	local snap=$1 suite arch comp
	fsnap=$snap
	for suite in "${suites[@]}"; do
		fetch "dists/$suite/InRelease" "$work/InRelease.$snap.$suite" || { echo "ubuntu-snapshot-bump: cannot fetch dists/$suite/InRelease at $snap" >&2; return 1; }
		if ((!no_gpg)); then
			# Parse only the signed text gpgv writes out: lines outside the clearsigned block (before
			# BEGIN PGP SIGNED MESSAGE or after the signature) do not affect gpgv's verdict, so the raw
			# file could carry an unsigned SHA256 list that release_hash would otherwise read.
			rm -f "$work/Release.$snap.$suite"
			gpgv --keyring "$keyring" --output "$work/Release.$snap.$suite" "$work/InRelease.$snap.$suite" >"$work/gpgv.out" 2>&1 || { cat "$work/gpgv.out" >&2; echo "ubuntu-snapshot-bump: InRelease of $suite does not verify" >&2; return 1; }
		else
			# --no-gpg (fixtures only): nothing is verified, the file is read as it is.
			cp "$work/InRelease.$snap.$suite" "$work/Release.$snap.$suite"
		fi
	done
	for arch in "${ARCHES[@]}"; do
		: >"$work/idx.$snap.$arch"
		for suite in "${suites[@]}"; do
			for comp in "${comps[@]}"; do
				append_index "$snap" "$suite" "$comp" "$arch" >>"$work/idx.$snap.$arch" || return 1
			done
		done
	done
}

# closure IDX: "name<TAB>version" for the pinned packages and everything they depend on (Depends and
# Pre-Depends, first alternative, highest version; a virtual name resolves to its first provider by
# name). apt installs the highest candidate of each, so this is what the snapshot decides for the image.
# It over-approximates (packages the base image already has are included), which only means a
# snapshot move is proposed when a package the image may contain got newer.
closure() {
	local idx=$1 n v d p pv first new=1 recs
	local -A want=() ver=() deps=() prov=()
	for n in "${names[@]}"; do want[$n]=1; done
	while ((new)); do
		new=0 ver=() deps=() prov=()
		recs=$(index_records "${!want[*]}" <"$idx")
		while IFS=$'\t' read -r n v d p; do
			[[ -n $n ]] || continue
			if [[ -n ${want[$n]:-} ]] && { [[ -z ${ver[$n]:-} ]] || dpkg_gt "$v" "${ver[$n]}"; }; then
				ver[$n]=$v
				deps[$n]=$d
			fi
			for pv in $p; do
				[[ -n ${want[$pv]:-} ]] && prov[$pv]+=" $n"
			done
		done <<<"$recs"
		for n in "${!ver[@]}"; do
			for d in ${deps[$n]}; do
				[[ -n ${want[$d]:-} ]] || { want[$d]=1; new=1; }
			done
		done
		for pv in "${!want[@]}"; do
			{ [[ -n ${ver[$pv]:-} ]] || [[ -z ${prov[$pv]:-} ]]; } && continue
			first=$(tr ' ' '\n' <<<"${prov[$pv]}" | sed '/^$/d' | sort | head -n 1)
			[[ -n ${want[$first]:-} ]] || { want[$first]=1; new=1; }
		done
	done
	for n in "${!ver[@]}"; do printf '%s\t%s\n' "$n" "${ver[$n]}"; done | sort
}

# load_versions ARRAY FILE: fills the associative array ARRAY from a closure file.
load_versions() {
	local -n arr=$1
	local n v
	arr=()
	# shellcheck disable=SC2034 # arr is the caller's array, filled through the nameref
	while IFS=$'\t' read -r n v; do [[ -n $n ]] && arr["$n"]=$v; done <"$2"
}

# --- usage ----------------------------------------------------------------------------------------

usage() {
	sed -n '4,60p' "${BASH_SOURCE[0]}" | sed -n '/^# Usage/,/^# Exit codes/p' | sed 's/^# \{0,1\}//' >&2
	exit 2
}

mode='' snapshot_arg='' today='' base='https://snapshot.ubuntu.com/ubuntu'
dockerfile=flavors/opencode/Dockerfile readme=flavors/opencode/README.md
keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg no_gpg=0 usn_dir='' usn_commit='' report=''
while (($#)); do
	case $1 in
	--check | --write | --print-snapshot) mode=${1#--} ;;
	--snapshot) snapshot_arg=${2:?}; shift ;;
	--today) today=${2:?}; shift ;;
	--base-url) base=${2:?}; shift ;;
	--dockerfile) dockerfile=${2:?}; shift ;;
	--readme) readme=${2:?}; shift ;;
	--keyring) keyring=${2:?}; shift ;;
	--no-gpg) no_gpg=1 ;;
	--usn-dir) usn_dir=${2:?}; shift ;;
	--usn-commit) usn_commit=${2:?}; shift ;;
	--report) report=${2:?}; shift ;;
	*) usage ;;
	esac
	shift
done
[[ -n $mode ]] || usage
base=${base%/}

cd "$(dirname "${BASH_SOURCE[0]}")/.."

for t in dpkg xz gzip sha256sum jq; do
	command -v "$t" >/dev/null || { echo "ubuntu-snapshot-bump: $t is required" >&2; exit 1; }
done

now=$(now_epoch "$today")

# The snapshot: today - 3 days, or an explicit one that is at least that old.
pick=$(pick_snapshot "$now")
if [[ -n $snapshot_arg ]]; then
	ts=$(snapshot_epoch "$snapshot_arg") || { echo "ubuntu-snapshot-bump: bad snapshot '$snapshot_arg' (want YYYYMMDDTHHMMSSZ)" >&2; exit 1; }
	if ((now - ts < MIN_AGE_DAYS * 86400)); then
		echo "ubuntu-snapshot-bump: snapshot $snapshot_arg is younger than $MIN_AGE_DAYS days (ADR 0079); the newest allowed today is $pick. The security fast path is a human PR with the dependency-age waiver." >&2
		exit 1
	fi
	snapshot=$snapshot_arg
else
	snapshot=$pick
fi

if [[ $mode == print-snapshot ]]; then
	echo "$snapshot"
	exit 0
fi

[[ -f $dockerfile ]] || { echo "ubuntu-snapshot-bump: $dockerfile not found" >&2; exit 1; }
cur_snapshot=$(sed -nE 's/^ARG UBUNTU_SNAPSHOT=([0-9]{8}T[0-9]{6}Z)[[:space:]]*$/\1/p' "$dockerfile")
[[ -n $cur_snapshot ]] || { echo "ubuntu-snapshot-bump: no 'ARG UBUNTU_SNAPSHOT=' in $dockerfile" >&2; exit 1; }

if [[ $cur_snapshot > $snapshot ]]; then
	echo "snapshot $cur_snapshot in $dockerfile is newer than $snapshot: nothing to do."
	exit 0
fi
if [[ $cur_snapshot == "$snapshot" ]]; then
	echo "snapshot $cur_snapshot in $dockerfile is already the target: no pin changes."
	exit 0
fi

mapfile -t pin_lines < <(parse_pins "$dockerfile")
((${#pin_lines[@]} > 0)) || { echo "ubuntu-snapshot-bump: no apt pins found in $dockerfile" >&2; exit 1; }
declare -A pinned
names=()
for l in "${pin_lines[@]}"; do
	pinned[${l%%$'\t'*}]=${l#*$'\t'}
	names+=("${l%%$'\t'*}")
done

suites_re='Suites: ([a-z -]+)\\nComponents: ([a-z ]+)\\n'
if ! [[ $(<"$dockerfile") =~ $suites_re ]]; then
	echo "ubuntu-snapshot-bump: cannot read the Suites and Components of the deb822 source in $dockerfile" >&2
	exit 1
fi
read -ra suites <<<"${BASH_REMATCH[1]}"
read -ra comps <<<"${BASH_REMATCH[2]}"

work=$(mktemp -d "${TMPDIR:-/tmp}/ubuntu-snapshot-bump.XXXXXX") || exit 1
trap 'rm -rf "$work"' EXIT

echo "snapshot: $cur_snapshot -> $snapshot (base $base)"
if ((no_gpg)); then
	echo "WARNING: --no-gpg: InRelease signatures are NOT verified (index hashes still are)" >&2
elif [[ ! -r $keyring ]] || ! command -v gpgv >/dev/null; then
	echo "ubuntu-snapshot-bump: gpgv or the keyring $keyring is missing; refusing to trust unsigned indexes (--no-gpg for fixtures)" >&2
	exit 1
fi

load_snapshot "$snapshot" || exit 1
load_snapshot "$cur_snapshot" || { echo "ubuntu-snapshot-bump: the current snapshot $cur_snapshot is needed to compare dependencies" >&2; exit 1; }

# shellcheck disable=SC2034 # filled by load_versions and read through namerefs
declare -A new_amd64 new_arm64 cur_amd64 cur_arm64
for arch in "${ARCHES[@]}"; do
	closure "$work/idx.$snapshot.$arch" >"$work/closure.new.$arch"
	closure "$work/idx.$cur_snapshot.$arch" >"$work/closure.cur.$arch"
	load_versions "new_$arch" "$work/closure.new.$arch"
	load_versions "cur_$arch" "$work/closure.cur.$arch"
done

# Every pin must exist on both architectures with the same version, and never go backwards.
bad=0 changed=()
declare -A cand
for n in "${names[@]}"; do
	a=${new_amd64[$n]:-} b=${new_arm64[$n]:-}
	if [[ -z $a || -z $b ]]; then
		echo "ERROR: $n is missing from the snapshot (amd64: ${a:-none}, arm64: ${b:-none})" >&2
		bad=1
		continue
	fi
	if [[ $a != "$b" ]]; then
		echo "ERROR: $n resolves to $a on amd64 and $b on arm64; a pin is one version for both platforms" >&2
		bad=1
		continue
	fi
	if dpkg_gt "${pinned[$n]}" "$a"; then
		echo "ERROR: $n is pinned at ${pinned[$n]}, newer than the snapshot's $a: the snapshot is older than the pin" >&2
		bad=1
		continue
	fi
	cand[$n]=$a
	[[ $a == "${pinned[$n]}" ]] || changed+=("$n")
done
((bad == 0)) || exit 1

# The dependencies the pins pull in (any other source package: python3.12 behind python3, curl and
# expat behind git, ...) are not pinned: apt takes them from whatever snapshot the ARG names. So a
# dependency that is newer in the new snapshot than in the current one is a change on its own, and the
# ARG moves for it. A dependency absent from the current snapshot is new (listed, no USN lookup).
declare -A pinset=() dep_old=() dep_new=()
dep_changed=()
for n in "${names[@]}"; do pinset[$n]=1; done
for arch in "${ARCHES[@]}"; do
	declare -n nw="new_$arch" cu="cur_$arch"
	for n in "${!nw[@]}"; do
		[[ -z ${pinset[$n]:-} ]] || continue
		if [[ -z ${cu[$n]:-} ]]; then
			old=none
		elif dpkg_gt "${nw[$n]}" "${cu[$n]}"; then
			old=${cu[$n]}
		else
			continue
		fi
		if [[ -z ${dep_new[$n]:-} ]]; then
			dep_changed+=("$n")
			dep_old[$n]=$old
			dep_new[$n]=${nw[$n]}
		elif [[ ${dep_new[$n]} != "${nw[$n]}" ]]; then
			dep_new[$n]+=" ($arch: ${nw[$n]})"
		fi
	done
	unset -n nw cu
done
mapfile -t dep_changed < <(printf '%s\n' "${dep_changed[@]}" | sort)
[[ -n ${dep_changed[0]:-} ]] || dep_changed=()

chg_names=("${changed[@]}")
for n in "${dep_changed[@]}"; do [[ ${dep_old[$n]} == none ]] || chg_names+=("$n"); done

printf '%-18s %-34s %-34s\n' package pinned candidate
for n in "${names[@]}"; do
	printf '%-18s %-34s %-34s%s\n' "$n" "${pinned[$n]}" "${cand[$n]}" "$([[ ${cand[$n]} == "${pinned[$n]}" ]] || echo '  *')"
done

if ((${#dep_changed[@]} > 0)); then
	echo
	printf '%-34s %-34s %-34s\n' 'dependency (not pinned)' 'current snapshot' 'new snapshot'
	for n in "${dep_changed[@]}"; do printf '%-34s %-34s %-34s\n' "$n" "${dep_old[$n]}" "${dep_new[$n]}"; done
fi

# --- USN annotation (best effort) --------------------------------------------------------------------

usn_state='' usn_rows="$work/usn.tsv"
usn_lookup() {
	local dir=$usn_dir files="$work/usn.files"
	[[ -n $dir ]] || { usn_state="skipped: no --usn-dir given"; return; }
	[[ -d $dir/osv/usn ]] && dir=$dir/osv/usn
	[[ -d $dir ]] || { usn_state="FAILED: $usn_dir has no osv/usn directory"; return; }
	find "$dir" -maxdepth 1 -name 'USN-*.json' -print0 >"$files"
	[[ -s $files ]] || { usn_state="FAILED: no USN-*.json under $dir"; return; }
	local names_json
	names_json=$(printf '%s\n' "${chg_names[@]}" | jq -R . | jq -sc .)
	# One row per (USN, binary): id, published, binary, binary_version, CVEs, Ubuntu priorities.
	# shellcheck disable=SC2016 # the jq program
	if ! xargs -0 jq -r --argjson names "$names_json" '
		. as $r | [ $r.affected[]? | select(.package.ecosystem == "Ubuntu:24.04:LTS") | . as $a
			| ($a.database_specific.cves_map.cves // []) as $cves
			| $a.ecosystem_specific.binaries[]? | select(.binary_name as $n | $names | index($n))
			| [$r.id, $r.published, .binary_name, .binary_version,
			   ([$cves[].id] | join(",")),
			   ([$cves[].severity[]? | select(.type == "Ubuntu") | .score] | unique | join(","))] ] | .[] | @tsv' <"$files" >"$usn_rows" 2>"$work/usn.err"; then
		usn_state="FAILED: jq over $dir: $(head -c 300 "$work/usn.err")"
		return
	fi
	usn_state="ok"
}

# usn_for PKG OLD NEW: report lines for the USNs whose fixed binary version is in (OLD, NEW].
usn_for() {
	local id pub bin ver cves sev
	[[ $usn_state == ok ]] || return 0
	while IFS=$'\t' read -r id pub bin ver cves sev; do
		[[ $bin == "$1" ]] || continue
		if dpkg_gt "$ver" "$2" && ! dpkg_gt "$ver" "$3"; then
			# usn_for runs in a command substitution, so the high/critical flag is a file, not a variable.
			[[ ${sev,,} =~ high|critical ]] && : >"$work/usn.high-priority"
			printf '%s (%s, fixes %s%s%s)\n' "$id" "${pub%%T*}" "$ver" "${sev:+, priority $sev}" "${cves:+, $cves}"
		fi
	done < <(sort -u "$usn_rows")
	return 0
}

if ((${#chg_names[@]} > 0)); then
	usn_lookup
	echo "USN lookup: $usn_state"
fi

if [[ -n $report ]]; then
	{
		echo "Ubuntu snapshot \`$cur_snapshot\` -> \`$snapshot\` (midnight UTC, at least $MIN_AGE_DAYS days old at the time of the run; ADR 0079)."
		echo
		echo "| Package | Pinned | New | USNs fixed in between |"
		echo "| --- | --- | --- | --- |"
		for n in "${names[@]}"; do
			[[ ${cand[$n]} == "${pinned[$n]}" ]] && continue
			u=$(usn_for "$n" "${pinned[$n]}" "${cand[$n]}" | paste -sd ';' - | sed 's/;/<br>/g')
			if [[ $usn_state != ok ]]; then
				u="lookup $usn_state"
			elif [[ -z $u ]]; then
				u="none found (noble-updates, or a security upload without a notice)"
			fi
			echo "| \`$n\` | \`${pinned[$n]}\` | \`${cand[$n]}\` | $u |"
		done
		echo
		if ((${#dep_changed[@]} > 0)); then
			echo "Dependencies the pins pull in (not pinned; apt takes them from the snapshot, so these moved with the ARG):"
			echo
			echo "| Package | Current snapshot | New snapshot | USNs fixed in between |"
			echo "| --- | --- | --- | --- |"
			for n in "${dep_changed[@]}"; do
				if [[ ${dep_old[$n]} == none ]]; then
					u="new dependency (no lookup)"
				else
					u=$(usn_for "$n" "${dep_old[$n]}" "${dep_new[$n]%% *}" | paste -sd ';' - | sed 's/;/<br>/g')
					if [[ $usn_state != ok ]]; then
						u="lookup $usn_state"
					elif [[ -z $u ]]; then
						u="none found (noble-updates, or a security upload without a notice)"
					fi
				fi
				echo "| \`$n\` | \`${dep_old[$n]}\` | \`${dep_new[$n]}\` | $u |"
			done
			echo
		fi
		echo "Unchanged pins: $(for n in "${names[@]}"; do [[ ${cand[$n]} == "${pinned[$n]}" ]] && printf '%s ' "$n"; done)"
		echo
		echo "USN source: canonical/ubuntu-security-notices at \`${usn_commit:-unknown}\`; lookup: $usn_state."
		echo "InRelease signatures: $( ((no_gpg)) && echo 'NOT verified (--no-gpg)' || echo 'verified with gpgv against the Ubuntu archive keyring'); Packages indexes: SHA256 checked against InRelease."
		if [[ $usn_state == ok && -e "$work/usn.high-priority" ]]; then
			echo
			echo "**A matched USN carries an Ubuntu priority of high or critical** (check which package): the security fast path of ADR 0079 is a human decision and this PR does not take it."
		fi
	} >"$report"
fi

if ((${#changed[@]} == 0 && ${#dep_changed[@]} == 0)); then
	echo "no pin changes and no newer dependency: the snapshot stays $cur_snapshot."
	exit 0
fi

if [[ $mode == check ]]; then
	echo "${#changed[@]} pin(s) and ${#dep_changed[@]} dependenc(ies) would change; pins: ${changed[*]:-none}"
	exit 10
fi

# --- --write: exactly the snapshot ARG, the changed pins and the README's snapshot ID --------------------

rewrite_dockerfile() {
	local line n out="$work/Dockerfile.new"
	: >"$out"
	while IFS= read -r line || [[ -n $line ]]; do
		if [[ $line == "ARG UBUNTU_SNAPSHOT=$cur_snapshot" ]]; then
			line="ARG UBUNTU_SNAPSHOT=$snapshot"
		else
			for n in "${changed[@]}"; do
				if [[ $line =~ ^([[:space:]]+)"$n"=([^[:space:]\\]+)(.*)$ && ${BASH_REMATCH[2]} == "${pinned[$n]}" ]]; then
					line="${BASH_REMATCH[1]}$n=${cand[$n]}${BASH_REMATCH[3]}"
					break
				fi
			done
		fi
		printf '%s\n' "$line" >>"$out"
	done <"$dockerfile"
	cat "$out" >"$dockerfile"
}

rewrite_readme() {
	[[ -f $readme ]] || { echo "ubuntu-snapshot-bump: $readme not found" >&2; return 1; }
	grep -qF "$cur_snapshot" "$readme" || { echo "ubuntu-snapshot-bump: $readme does not mention $cur_snapshot" >&2; return 1; }
	local line out="$work/README.new"
	: >"$out"
	while IFS= read -r line || [[ -n $line ]]; do
		printf '%s\n' "${line//"$cur_snapshot"/$snapshot}" >>"$out"
	done <"$readme"
	cat "$out" >"$readme"
}

rewrite_readme
rewrite_dockerfile
echo "wrote $dockerfile and $readme: snapshot $snapshot; pins: ${changed[*]:-none}; ${#dep_changed[@]} dependency change(s)"
exit 10
