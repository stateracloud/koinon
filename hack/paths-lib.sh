#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Reader of hack/paths.rules (the one source of the path rules; its header says what each kind of
# rule means). Sourced by hack/ci-changes.sh, hack/needs-e2e.sh, hack/preflight.sh and
# hack/lane-gates.sh. HACK_PATHS_RULES replaces the rules file (hack/paths-lib-test.sh).
#
#   paths_load                  parse the rules; on a bad line print why to stderr and return 2
#   paths_match <kind> <name> <path>   the path matches some rule of that kind and name
#   paths_job_triggers <job> <path>    the path can affect the CI job (job, except, keep)
#   paths_job_any <job>         reads paths on stdin; success when one triggers the job
#   paths_jobs                  the job names, in file order
#   paths_raw <kind> <name>     the rest of every such line (lane, e2e-binary, script-check,
#                               script-skip), one per line
#   paths_in_graph <dirs> <path>   the file's directory, or an ancestor of it, is in the list
#   paths_script_checks         every script-check line as `<inputs>TAB<command>` (the line is
#                               `inputs=<a>,<b>,... <command>`: repo paths, a trailing `/` is a
#                               directory, or `guard` = always runs; hack/paths-test.sh fails on a
#                               line without `inputs=`)
#   paths_inputs_hit <inputs> <path>   the changed path is one of the inputs (guard: always)
#   paths_script_refs <file>    the existing repo paths a script names (its direct inputs); the
#                               tripwire hack/paths-test.sh checks each against the annotation
#
# Needs bash 5 (ADR 0090): associative arrays, `declare -g`.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

declare -gA PATHS_RE=() PATHS_RAW=()
declare -ga PATHS_JOBS=()

# paths_regex_status <re>: the status of [[ =~ ]] on an empty string: 2 is a pattern that does not
# compile, 1 a mere non-match. Never fails the caller under `set -e`.
paths_regex_status() {
	[[ "" =~ $1 ]]
	return $?
}

paths_load() {
	local file=${HACK_PATHS_RULES:-$(dirname "${BASH_SOURCE[0]}")/paths.rules}
	local line kind name rest n=0 j seen rc
	PATHS_RE=() PATHS_RAW=() PATHS_JOBS=()
	[[ -r $file ]] || { echo "paths: cannot read $file" >&2; return 2; }
	while IFS= read -r line || [[ -n $line ]]; do
		n=$((n + 1))
		line=${line#"${line%%[![:space:]]*}"} # trim leading and trailing blanks
		line=${line%"${line##*[![:space:]]}"}
		[[ -z $line || $line == '#'* ]] && continue
		kind=${line%%[[:space:]]*}
		rest=${line#"$kind"}
		rest=${rest#"${rest%%[![:space:]]*}"}
		name=${rest%%[[:space:]]*}
		rest=${rest#"$name"}
		rest=${rest#"${rest%%[![:space:]]*}"}
		if [[ -z $name || -z $rest ]]; then
			echo "paths: $file:$n: want '<kind> <name> <rest>': $line" >&2
			return 2
		fi
		case $kind in
		everything | job | except | keep | gate | e2e-needed | e2e-forced | e2e-ignored)
			# A pattern that does not compile is exit 2 of [[ =~ ]]; fail closed, never a silent no-match.
			rc=0
			paths_regex_status "$rest" || rc=$?
			if [[ $rc -eq 2 ]]; then
				echo "paths: $file:$n: bad regular expression: $rest" >&2
				return 2
			fi
			PATHS_RE["$kind $name"]+="${PATHS_RE["$kind $name"]:+|}($rest)"
			if [[ $kind == job ]]; then
				seen=0
				for j in "${PATHS_JOBS[@]}"; do [[ $j == "$name" ]] && seen=1; done
				[[ $seen == 1 ]] || PATHS_JOBS+=("$name")
			fi
			;;
		lane | e2e-binary | script-check | script-skip)
			PATHS_RAW["$kind $name"]+="${PATHS_RAW["$kind $name"]:+$'\n'}$rest"
			;;
		*)
			echo "paths: $file:$n: unknown kind '$kind'" >&2
			return 2
			;;
		esac
	done <"$file"
	[[ ${#PATHS_JOBS[@]} -gt 0 && -n ${PATHS_RE["everything -"]:-} ]] || { echo "paths: $file has no job rules or no everything rule" >&2; return 2; }
}

paths_match() { # <kind> <name> <path>
	local re=${PATHS_RE["$1 $2"]:-}
	[[ -n $re && $3 =~ $re ]]
}

paths_job_triggers() { # <job> <path>
	paths_match job "$1" "$2" || return 1
	if paths_match except "$1" "$2"; then paths_match keep "$1" "$2"; return; fi
	return 0
}

paths_job_any() { # <job>; paths on stdin
	local p
	while IFS= read -r p; do
		[[ -n $p ]] || continue
		paths_job_triggers "$1" "$p" && return 0
	done
	return 1
}

paths_jobs() { printf '%s\n' "${PATHS_JOBS[@]}"; }

paths_raw() { printf '%s\n' "${PATHS_RAW["$1 $2"]-}"; }

paths_in_graph() { # <newline-separated dirs> <path>
	local dirs=$1 d=${2%/*}
	[[ $d != "$2" ]] || return 1 # a file in the repository root
	while [[ $d != . && -n $d ]]; do
		grep -qxF -- "$d" <<<"$dirs" && return 0
		[[ $d == */* ]] || break
		d=${d%/*}
	done
	return 1
}

paths_script_checks() { # every script-check line as <inputs><TAB><command>; an unannotated line has inputs `-`
	local l inp
	while IFS= read -r l; do
		[[ -n $l ]] || continue
		if [[ $l == inputs=* ]]; then
			inp=${l%% *}
			printf '%s\t%s\n' "${inp#inputs=}" "${l#"$inp"}" | sed $'s/\t */\t/'
		else
			printf -- '-\t%s\n' "$l"
		fi
	done < <(paths_raw script-check -)
}

paths_inputs_hit() { # <inputs: comma list or guard> <path>
	local i
	[[ $1 == guard ]] && return 0
	for i in ${1//,/ }; do
		if [[ $i == */ ]]; then [[ $2 == "$i"* ]] && return 0; else [[ $2 == "$i" ]] && return 0; fi
	done
	return 1
}

paths_script_refs() { # <file>: existing repo paths the script names (a directory ends in /)
	local f=$1 d=${1%/*} t
	[[ $d != "$f" ]] || d=.
	grep -oE '[A-Za-z0-9_.@+/-]+' "$f" | sort -u | while IFS= read -r t; do
		t=${t#./}
		[[ -n $t && $t != /* && $t != *..* && $t != */ ]] || continue
		if [[ $t == */* ]]; then
			if [[ -e $t ]]; then [[ -d $t ]] && echo "$t/" || echo "$t"
			elif [[ -e $d/$t ]]; then [[ -d $d/$t ]] && echo "$d/$t/" || echo "$d/$t"; fi
		elif [[ $t == *.* && -f $d/$t ]]; then
			echo "$d/$t"
		fi
	done
}
