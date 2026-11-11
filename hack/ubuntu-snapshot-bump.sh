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
#   hack/ubuntu-snapshot-bump.sh --check [options]     exit 0: no pin changes, 10: a pin would change
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

# packages_versions WANTED...: reads a Packages index on stdin, prints "name<TAB>version" for wanted names.
packages_versions() {
	awk -v wanted=" $* " '
		/^Package: / { p = $2; next }
		/^Version: / { if (index(wanted, " " p " ")) print p "\t" $2; next }'
}

dpkg_gt() { dpkg --compare-versions "$1" gt "$2"; }

# fetch REL DEST: one file below $base/$snapshot, from a URL or a directory.
fetch() {
	local rel=$1 dest=$2
	if [[ $base == http://* || $base == https://* ]]; then
		curl --silent --show-error --fail --location --retry 3 --retry-delay 2 --max-time 120 -o "$dest" "$base/$snapshot/$rel"
	else
		cp "${base#file://}/$snapshot/$rel" "$dest"
	fi
}

# release_hash INRELEASE PATH: the SHA256 InRelease lists for PATH (relative to the suite).
release_hash() {
	awk -v want="$2" '
		/^SHA256:/ { s = 1; next }
		/^[^ ]/ { s = 0 }
		s && $3 == want { print $1; exit }' "$1"
}

# index_candidates SUITE COMP ARCH: "name<TAB>version" from one verified Packages index.
index_candidates() {
	local suite=$1 comp=$2 arch=$3 rel ext got want f
	rel="$comp/binary-$arch/Packages"
	for ext in .xz .gz ""; do
		want=$(release_hash "$work/InRelease.$suite" "$rel$ext")
		[[ -n $want ]] && break
	done
	[[ -n $want ]] || { echo "$suite: InRelease lists no $rel{.xz,.gz,}" >&2; return 1; }
	f="$work/Packages.$suite.$comp.$arch"
	fetch "dists/$suite/$rel$ext" "$f$ext" || { echo "$suite: cannot fetch $rel$ext" >&2; return 1; }
	got=$(sha256sum "$f$ext")
	got=${got%% *}
	[[ $got == "$want" ]] || { echo "$suite: $rel$ext SHA256 $got does not match InRelease ($want)" >&2; return 1; }
	case $ext in
	.xz) xz -dc "$f$ext" ;;
	.gz) gzip -dc "$f$ext" ;;
	*) cat "$f$ext" ;;
	esac | packages_versions "${names[@]}"
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

declare -A best_amd64 best_arm64
for suite in "${suites[@]}"; do
	fetch "dists/$suite/InRelease" "$work/InRelease.$suite" || { echo "ubuntu-snapshot-bump: cannot fetch dists/$suite/InRelease at $snapshot" >&2; exit 1; }
	if ((!no_gpg)); then
		gpgv --keyring "$keyring" "$work/InRelease.$suite" >"$work/gpgv.out" 2>&1 || { cat "$work/gpgv.out" >&2; echo "ubuntu-snapshot-bump: InRelease of $suite does not verify" >&2; exit 1; }
	fi
	for arch in "${ARCHES[@]}"; do
		declare -n best="best_$arch"
		for comp in "${comps[@]}"; do
			lines=$(index_candidates "$suite" "$comp" "$arch") || exit 1
			while IFS=$'\t' read -r n v; do
				[[ -n $n ]] || continue
				if [[ -z ${best[$n]:-} ]] || dpkg_gt "$v" "${best[$n]}"; then best["$n"]=$v; fi
			done <<<"$lines"
		done
		unset -n best
	done
done

# Every pin must exist on both architectures with the same version, and never go backwards.
bad=0 changed=()
declare -A cand
for n in "${names[@]}"; do
	a=${best_amd64[$n]:-} b=${best_arm64[$n]:-}
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

printf '%-18s %-34s %-34s\n' package pinned candidate
for n in "${names[@]}"; do
	printf '%-18s %-34s %-34s%s\n' "$n" "${pinned[$n]}" "${cand[$n]}" "$([[ ${cand[$n]} == "${pinned[$n]}" ]] || echo '  *')"
done

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
	names_json=$(printf '%s\n' "${changed[@]}" | jq -R . | jq -sc .)
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
			printf '%s (%s, fixes %s%s%s)\n' "$id" "${pub%%T*}" "$ver" "${sev:+, priority $sev}" "${cves:+, $cves}"
		fi
	done < <(sort -u "$usn_rows")
	return 0
}

if ((${#changed[@]} > 0)); then
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
		echo "Unchanged pins: $(for n in "${names[@]}"; do [[ ${cand[$n]} == "${pinned[$n]}" ]] && printf '%s ' "$n"; done)"
		echo
		echo "USN source: canonical/ubuntu-security-notices at \`${usn_commit:-unknown}\`; lookup: $usn_state."
		echo "InRelease signatures: $( ((no_gpg)) && echo 'NOT verified (--no-gpg)' || echo 'verified with gpgv against the Ubuntu archive keyring'); Packages indexes: SHA256 checked against InRelease."
		if [[ $usn_state == ok ]] && grep -qiE 'high|critical' "$usn_rows"; then
			echo
			echo "**A matched USN carries an Ubuntu priority of high or critical** (check which package): the security fast path of ADR 0079 is a human decision and this PR does not take it."
		fi
	} >"$report"
fi

if ((${#changed[@]} == 0)); then
	echo "no pin changes: the snapshot stays $cur_snapshot."
	exit 0
fi

if [[ $mode == check ]]; then
	echo "${#changed[@]} pin(s) would change: ${changed[*]}"
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
echo "wrote $dockerfile and $readme: ${changed[*]}"
exit 10
