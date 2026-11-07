#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# List working notes that are due for review (old issue 527). Two modes:
#
#   notes-expiry.sh DIR [--exclude NAME]... [--today YYYY-MM-DD]
#       Markdown files in DIR (not recursive) whose frontmatter has `expires: YYYY-MM-DD` or
#       `review_by: YYYY-MM-DD` in the past, plus files with neither field unless they carry
#       `durable: true`. A key may be indented (nested under `metadata:`). Only the first
#       `---` block is read.
#   notes-expiry.sh --log FILE --days N [--today YYYY-MM-DD]
#       Bullets of the form `- YYYY-MM-DD ...` older than N days.
#
# Output, one line per item, tab-separated: <path or line number> <date> <first 80 characters>.
# The date column is `no-expiry` for a file without a field. Exit 0: nothing listed; 1: something
# listed; 2: usage error. Pure bash arithmetic on dates, so it behaves the same with BSD and GNU date.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

usage() {
	cat >&2 <<'EOF'
usage: notes-expiry.sh DIR [--exclude NAME]... [--today YYYY-MM-DD]
       notes-expiry.sh --log FILE --days N [--today YYYY-MM-DD]
EOF
	exit 2
}

# days_since_epoch YYYY-MM-DD -> stdout (civil-from-days algorithm), non-zero when malformed.
days_since_epoch() {
	local s=$1 y m d era yoe doy doe
	[[ $s =~ ^([0-9]{4})-([0-9]{2})-([0-9]{2})$ ]] || return 1
	y=$((10#${BASH_REMATCH[1]})) m=$((10#${BASH_REMATCH[2]})) d=$((10#${BASH_REMATCH[3]}))
	((m >= 1 && m <= 12 && d >= 1 && d <= 31)) || return 1
	((m <= 2)) && y=$((y - 1))
	era=$((y / 400))
	yoe=$((y - era * 400))
	doy=$(((153 * (m > 2 ? m - 3 : m + 9) + 2) / 5 + d - 1))
	doe=$((yoe * 365 + yoe / 4 - yoe / 100 + doy))
	echo $((era * 146097 + doe - 719468))
}

trunc80() { # collapse whitespace, cut to 80 characters
	local t
	t=$(printf '%s' "$1" | tr '\t' ' ')
	printf '%s' "${t:0:80}"
}

dir="" logfile="" days="" today="" excludes=()
while (($#)); do
	case $1 in
	--log) (($# >= 2)) || usage; logfile=$2; shift 2 ;;
	--days) (($# >= 2)) || usage; days=$2; shift 2 ;;
	--today) (($# >= 2)) || usage; today=$2; shift 2 ;;
	--exclude) (($# >= 2)) || usage; excludes+=("$2"); shift 2 ;;
	-h | --help) usage ;;
	-*) echo "notes-expiry: unknown option $1" >&2; usage ;;
	*) [[ -z $dir ]] || usage; dir=$1; shift ;;
	esac
done

[[ -n $today ]] || today=$(date +%Y-%m-%d)
today_n=$(days_since_epoch "$today") || { echo "notes-expiry: bad --today '$today'" >&2; exit 2; }

if [[ -n $logfile ]]; then
	[[ -z $dir && -n $days ]] || usage
	[[ $days =~ ^[0-9]+$ ]] || { echo "notes-expiry: --days needs a number" >&2; exit 2; }
	[[ -f $logfile ]] || { echo "notes-expiry: no such file: $logfile" >&2; exit 2; }
	found=0 n=0
	while IFS= read -r line || [[ -n $line ]]; do
		n=$((n + 1))
		[[ $line =~ ^-[[:space:]]+([0-9]{4}-[0-9]{2}-[0-9]{2})([[:space:]]|$)(.*) ]] || continue
		dt=${BASH_REMATCH[1]}
		dn=$(days_since_epoch "$dt") || continue
		if ((today_n - dn > days)); then
			printf '%s\t%s\t%s\n' "$n" "$dt" "$(trunc80 "${line#- }")"
			found=1
		fi
	done <"$logfile"
	exit "$found"
fi

[[ -n $dir && -z $days ]] || usage
[[ -d $dir ]] || { echo "notes-expiry: no such directory: $dir" >&2; exit 2; }

found=0
for f in "$dir"/*.md; do
	[[ -f $f ]] || continue
	base=${f##*/}
	skip=0
	for e in ${excludes[@]+"${excludes[@]}"}; do [[ $base == "$e" ]] && skip=1; done
	((skip)) && continue

	# Read the first frontmatter block.
	infm=0 durable=0 date="" desc="" first="" lineno=0
	while IFS= read -r line || [[ -n $line ]]; do
		lineno=$((lineno + 1))
		line=${line%$'\r'}
		if ((lineno == 1)); then
			[[ $line == "---" ]] && { infm=1; continue; }
		fi
		if ((infm)); then
			[[ $line == "---" ]] && { infm=0; continue; }
			if [[ $line =~ ^[[:space:]]*(expires|review_by):[[:space:]]*[\"\']?([^\"\'[:space:]#]*) ]]; then
				# the earliest of the two wins
				if [[ -z $date ]]; then date=${BASH_REMATCH[2]}; else
					a=$(days_since_epoch "$date") || a=-1
					b=$(days_since_epoch "${BASH_REMATCH[2]}") || b=-1
					((b < a)) && date=${BASH_REMATCH[2]}
				fi
			elif [[ $line =~ ^[[:space:]]*durable:[[:space:]]*[\"\']?true ]]; then
				durable=1
			elif [[ -z $desc && $line =~ ^description:[[:space:]]*[\"\']?(.*) ]]; then
				desc=${BASH_REMATCH[1]}
			fi
		elif [[ -z $first && -n $line && $line != "---" ]]; then
			first=$line
		fi
	done <"$f"

	text=${desc:-$first}
	[[ -n $text ]] || text=$base
	if [[ -n $date ]]; then
		if dn=$(days_since_epoch "$date"); then
			((dn < today_n)) || continue
		fi # a malformed date is listed: someone must fix it
		printf '%s\t%s\t%s\n' "$f" "$date" "$(trunc80 "$text")"
		found=1
	elif ((durable)); then
		continue
	else
		printf '%s\tno-expiry\t%s\n' "$f" "$(trunc80 "$text")"
		found=1
	fi
done
exit "$found"
