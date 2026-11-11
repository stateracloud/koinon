#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Dependency cooldown check (ADR 0079): every version a pull request adds or changes must have
# been published at least MIN_AGE_DAYS (3) days ago. Looks at the lines the PR adds, compared with
# the merge base, in:
#   - go.mod: requires, and the `go` directive (toolchain release)
#   - workflows: `uses: owner/repo@<sha>` (age of the commit, and of the release named in the
#     trailing `# vX.Y.Z` comment; the younger counts; a tag that does not point at the SHA warns)
#   - any non-doc file: image references pinned by digest (`name:tag@sha256:...`): the registry's
#     build time of the newest platform image
#   - Dockerfile `ADD` and `koinon-fetch` URLs of GitHub release assets, raw.githubusercontent.com tags and npm tarballs
#   - the `<TOOL>_VERSION` pins in hack/versions.env (the release's publish time)
#   - the ADR 0077 platform pins in test/e2e/kind/up.sh, and any line that carries a marker
#     `# dep-age: github=<owner/repo> [tag-prefix=<prefix>]` (use it where release/versions.yaml
#     or another file records a pin; the first version-looking token on the line is checked)
#   - chart dependencies in charts/*/Chart.yaml
#
# Timestamps are a guard against accidents (a bot or a person picking up yesterday's release), not
# proof. A publisher can back-date a Go tag or an image `created` field, and the check cannot see a
# release that was yanked or re-published. The security fast path and the other controls of
# ADR 0079 (pinning, provenance, review) do the real work. Only public endpoints are used, no
# secrets; GITHUB_TOKEN, if set, only raises the GitHub API rate limit.
#
# Usage:
#   hack/dep-age-check.sh [base-ref]      # check the changes since the merge base (default origin/main)
#   hack/dep-age-check.sh go MODULE VERSION | action OWNER/REPO SHA [TAG] | image REF
#                         | github OWNER/REPO TAG | npm PACKAGE VERSION | url URL
# Environment:
#   MIN_AGE_DAYS        minimum age in days (default 3)
#   DEP_AGE_WAIVER      set to the waiver's author (the PR label's actor) to report instead of fail

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."

MIN_AGE_DAYS=${MIN_AGE_DAYS:-3}
WAIVER=${DEP_AGE_WAIVER:-}

# Public endpoints only.
curl_opts=(--silent --show-error --fail --location --retry 3 --retry-delay 2 --max-time 30)
gh_headers=(-H "Accept: application/vnd.github+json")
[[ -n ${GITHUB_TOKEN:-} ]] && gh_headers+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")

fetch() { curl "${curl_opts[@]}" "$@"; }
gh_api() { fetch "${gh_headers[@]}" "https://api.github.com/$1"; }

# age_days <ISO 8601 time>: whole-and-tenth days between that time and now.
age_days() {
	jq -rn --arg t "$1" '
		def ts: sub("\\.[0-9]+"; "") | capture("^(?<d>.*?)(?<o>Z|[+-][0-9]{2}:[0-9]{2})$") as $c
			| ($c.d + "Z" | fromdateiso8601) as $u
			| if $c.o == "Z" then $u
			  else ($c.o | capture("^(?<s>[+-])(?<h>[0-9]{2}):(?<m>[0-9]{2})$")) as $z
			  | ($z.h | tonumber) * 3600 + ($z.m | tonumber) * 60 as $off
			  | if $z.s == "+" then $u - $off else $u + $off end end;
		((now - ($t | ts)) / 86400 * 10 | floor) / 10'
}

fails=0
checked=0
report() { # report <what> <age> <source> [note]
	local what=$1 age=$2 src=$3 note=${4:-} status=OK
	checked=$((checked + 1))
	if [[ $age == unknown ]]; then
		status=FAIL
	elif awk -v a="$age" -v m="$MIN_AGE_DAYS" 'BEGIN { exit !(a < m) }'; then
		status=FAIL
	fi
	if [[ $status == FAIL ]]; then
		if [[ -n $WAIVER ]]; then
			status=WAIVED
		else
			fails=$((fails + 1))
		fi
	fi
	local shown=$age
	[[ $age == unknown ]] || shown="${age}d"
	printf '%-7s %-58s age=%-9s %s%s\n' "$status" "$what" "$shown" "$src" "${note:+ ($note)}"
	[[ $status == WAIVED ]] && echo "::warning::dependency age waived for ${what} (age ${shown}, minimum ${MIN_AGE_DAYS}d) by ${WAIVER}" || true
	return 0
}

# --- sources ------------------------------------------------------------------------------------

# Go module proxy: the version's `.info` Time. Uppercase letters are escaped as !lowercase.
go_escape() { jq -rn --arg s "$1" '$s | gsub("(?<u>[A-Z])"; "!" + (.u | ascii_downcase))'; }
check_go() {
	local mod=$1 ver=$2 t
	t=$(fetch "https://proxy.golang.org/$(go_escape "$mod")/@v/$(go_escape "$ver").info" | jq -r .Time) || t=
	report "go $mod $ver" "$([[ -n $t ]] && age_days "$t" || echo unknown)" "proxy.golang.org ${t:-no answer}"
}

check_go_toolchain() { # the `go 1.x.y` directive: the toolchain release in the module proxy
	local ver=$1 t
	t=$(fetch "https://proxy.golang.org/golang.org/toolchain/@v/v0.0.1-go${ver}.linux-amd64.info" | jq -r .Time) || t=
	report "go directive $ver" "$([[ -n $t ]] && age_days "$t" || echo unknown)" "proxy.golang.org toolchain ${t:-no answer}"
}

# GitHub: release date of a tag, else the commit date of the tag.
github_tag_time() { # github_tag_time <owner/repo> <tag>: prints the time and the source
	local repo=$1 tag=$2 t
	if t=$(gh_api "repos/$repo/releases/tags/$tag" 2>/dev/null | jq -er '.published_at // empty'); then
		echo "$t release"
	elif t=$(gh_api "repos/$repo/commits/$tag" 2>/dev/null | jq -er '.commit.committer.date // empty'); then
		echo "$t commit"
	fi
}
check_github() {
	local repo=$1 tag=$2 out t kind
	out=$(github_tag_time "$repo" "$tag") || out=
	if [[ -n $out ]]; then
		read -r t kind <<<"$out"
		report "github $repo $tag" "$(age_days "$t")" "$kind $t"
	else
		report "github $repo $tag" unknown "no release or tag found"
	fi
}

check_action() { # check_action <owner/repo[/path]> <sha> [tag]
	local spec=$1 sha=$2 tag=${3:-} repo t rt out age note=
	repo=$(cut -d/ -f1,2 <<<"$spec")
	t=$(gh_api "repos/$repo/commits/$sha" | jq -r '.commit.committer.date') || t=
	if [[ -z $t ]]; then
		report "action $spec@${sha:0:12}" unknown "commit not found in $repo"
		return
	fi
	age=$(age_days "$t")
	if [[ -n $tag ]]; then
		if out=$(github_tag_time "$repo" "$tag") && [[ -n $out ]]; then
			read -r rt _ <<<"$out"
			local rage
			rage=$(age_days "$rt")
			# The younger of the commit and the release counts.
			if awk -v a="$rage" -v b="$age" 'BEGIN { exit !(a < b) }'; then age=$rage; t=$rt; fi
		fi
		local tagsha
		tagsha=$(gh_api "repos/$repo/commits/$tag" 2>/dev/null | jq -r '.sha // empty') || tagsha=
		if [[ -n $tagsha && $tagsha != "$sha" ]]; then
			note="tag $tag now points at ${tagsha:0:12}, not the pinned SHA: a moved tag?"
			echo "::warning::$spec: $note"
		fi
	fi
	report "action $spec@${sha:0:12}${tag:+ ($tag)}" "$age" "github $t" "$note"
}

check_npm() { # check_npm <package> <version>
	local pkg=$1 ver=$2 t
	t=$(fetch "https://registry.npmjs.org/${pkg//\//%2f}" | jq -r --arg v "$ver" '.time[$v] // empty') || t=
	report "npm $pkg $ver" "$([[ -n $t ]] && age_days "$t" || echo unknown)" "registry.npmjs.org ${t:-no answer}"
}

# check_url <url>: GitHub release assets, raw.githubusercontent.com tags and npm tarballs.
check_url() {
	local url=$1 m
	if [[ $url =~ ^https://github\.com/([^/]+/[^/]+)/releases/download/([^/]+)/ ]]; then
		check_github "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
	elif [[ $url =~ ^https://raw\.githubusercontent\.com/([^/]+/[^/]+)/(v?[0-9][^/]*)/ ]]; then
		check_github "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
	elif [[ $url =~ ^https://registry\.npmjs\.org/(.+)/-/[^/]+-([0-9][^/]*)\.tgz$ ]]; then
		m=${BASH_REMATCH[1]}
		check_npm "$m" "${BASH_REMATCH[2]}"
	else
		report "url $url" unknown "no known source for this host"
	fi
}

# Container registries. The Bearer challenge is answered anonymously. Dating order (#537, ADR 0079):
#   1. org.opencontainers.image.created on the manifest (or index / descriptor annotations)
#   2. the image config's `created` field (normal container images)
#   3. the registry's publish time, when the registry exposes one
# A reference that cannot be dated fails; it is never skipped (except reserved test domains).

# image_registry_host <host>: docker.io and index.docker.io are Docker Hub; the registry API is on
# registry-1.docker.io (#537). Bare names with no host already use that host.
image_registry_host() {
	case "${1%%:*}" in
	docker.io | index.docker.io) printf '%s\n' registry-1.docker.io ;;
	*) printf '%s\n' "$1" ;;
	esac
}

# oci_annotation_created <json>: org.opencontainers.image.created on a manifest, index, or
# descriptor; one ISO time per line, oldest to newest does not matter (caller picks newest).
oci_annotation_created() {
	jq -r '
		(.annotations["org.opencontainers.image.created"] // empty),
		(.manifests[]? | .annotations["org.opencontainers.image.created"] // empty)
	' <<<"$1" 2>/dev/null | grep -E . || true
}

image_manifest() { # image_manifest <host> <repo> <ref> -> JSON on stdout
	local host=$1 repo=$2 ref=$3 url hdr token realm service scope accept
	accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
	url="https://$host/v2/$repo/manifests/$ref"
	if curl "${curl_opts[@]}" -H "Accept: $accept" "$url" 2>/dev/null; then return 0; fi
	hdr=$(curl --silent --max-time 30 -I -H "Accept: $accept" "$url" | tr -d '\r' | grep -i '^www-authenticate:' || true)
	[[ -z $hdr ]] && return 1
	realm=$(sed -nE 's/.*realm="([^"]+)".*/\1/p' <<<"$hdr")
	service=$(sed -nE 's/.*service="([^"]+)".*/\1/p' <<<"$hdr")
	scope="repository:$repo:pull"
	token=$(fetch "$realm?service=$service&scope=$scope" | jq -r '.token // .access_token')
	fetch -H "Authorization: Bearer $token" -H "Accept: $accept" "$url"
}
image_blob() { # image_blob <host> <repo> <digest>
	local host=$1 repo=$2 digest=$3 hdr realm service token url="https://$1/v2/$2/blobs/$3"
	if curl "${curl_opts[@]}" "$url" 2>/dev/null; then return 0; fi
	hdr=$(curl --silent --max-time 30 -I "$url" | tr -d '\r' | grep -i '^www-authenticate:' || true)
	realm=$(sed -nE 's/.*realm="([^"]+)".*/\1/p' <<<"$hdr")
	service=$(sed -nE 's/.*service="([^"]+)".*/\1/p' <<<"$hdr")
	token=$(fetch "$realm?service=$service&scope=repository:$repo:pull" | jq -r '.token // .access_token')
	fetch -H "Authorization: Bearer $token" "$url"
}

# image_registry_published <host> <repo> <tag|digest>: a publish time the registry itself exposes.
# Docker Hub's tag API has last_updated; a HEAD Last-Modified on the manifest is used when present.
# The HTTP Date response header is not a publish time and is ignored.
image_registry_published() {
	local host=$1 repo=$2 ref=$3 t lm
	if [[ $host == registry-1.docker.io && $ref != sha256:* ]]; then
		t=$(fetch "https://hub.docker.com/v2/repositories/${repo}/tags/${ref}" 2>/dev/null | jq -r '.last_updated // empty' 2>/dev/null) || t=
		[[ -n $t && $t != null ]] && { printf '%s\n' "$t"; return 0; }
	fi
	lm=$(curl --silent --max-time 30 -I -H 'Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
		"https://$host/v2/$repo/manifests/$ref" 2>/dev/null | tr -d '\r' | grep -i '^last-modified:' | sed 's/^[Ll]ast-[Mm]odified:[[:space:]]*//' || true)
	[[ -z $lm ]] && return 1
	# RFC 7231 HTTP-date → ISO 8601 UTC for age_days.
	t=$(date -u -d "$lm" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || t=
	[[ -n $t ]] && printf '%s\n' "$t"
}

# consider_created <iso>: keep the newest ISO time in $newest (caller-declared).
consider_created() {
	local created=$1
	[[ -z $created || $created == null ]] && return 0
	if [[ -z $newest || $created > $newest ]]; then newest=$created; fi
}

check_image() { # check_image <name[:tag]@sha256:digest | name:tag>
	local ref=$1 name tag='' digest='' host repo first rest idx created newest='' m cfg d src=''
	ref=${ref#oci://}
	name=${ref%%@*}
	[[ $ref == *@* ]] && digest=${ref#*@}
	if [[ ${name##*/} == *:* ]]; then tag=${name##*:}; name=${name%:*}; fi
	first=${name%%/*}
	if [[ $name == */* && ($first == *.* || $first == *:* || $first == localhost) ]]; then
		host=$first; repo=${name#*/}
	else
		host=registry-1.docker.io; repo=$name
	fi
	host=$(image_registry_host "$host")
	# Official Hub images live under library/: bare `alpine` and `docker.io/alpine` both become
	# registry-1.docker.io/library/alpine. User/org paths already contain a slash.
	if [[ $host == registry-1.docker.io && $repo != */* ]]; then
		repo="library/$repo"
	fi
	# Reserved names (RFC 2606, RFC 6761) are test fixtures, never something a build pulls.
	case "${host%%:*}" in
	example.com | example.net | example.org | *.example.com | *.example.net | *.example.org | \
		*.example | *.test | *.invalid | *.localhost)
		echo "SKIP    image ${ref:0:70} (reserved test domain)"
		return 0
		;;
	esac
	rest=${digest:-${tag:-latest}}
	if ! idx=$(image_manifest "$host" "$repo" "$rest"); then
		report "image $ref" unknown "$host: manifest not readable anonymously"
		return 0
	fi
	if ! jq -e . >/dev/null 2>&1 <<<"$idx"; then
		report "image $ref" unknown "$host: manifest is not JSON"
		return 0
	fi
	# 1. OCI created annotation on the index / manifest (Helm chart artifacts, etc.).
	while read -r created; do
		[[ -z $created ]] && continue
		consider_created "$created"
		src=annotation
	done < <(oci_annotation_created "$idx")
	local configs=()
	if jq -e '.manifests' >/dev/null 2>&1 <<<"$idx"; then
		# Skip attestation manifests (platform "unknown").
		while read -r d; do
			[[ -z $d ]] && continue
			m=$(image_manifest "$host" "$repo" "$d") || continue
			jq -e . >/dev/null 2>&1 <<<"$m" || continue
			while read -r created; do
				[[ -z $created ]] && continue
				consider_created "$created"
				src=annotation
			done < <(oci_annotation_created "$m")
			cfg=$(jq -r '.config.digest // empty' <<<"$m" 2>/dev/null) || cfg=
			[[ -n $cfg && $cfg != null ]] && configs+=("$cfg")
		done < <(jq -r '.manifests[]? | select(.platform.os == "linux") | .digest // empty' <<<"$idx" 2>/dev/null)
	else
		cfg=$(jq -r '.config.digest // empty' <<<"$idx" 2>/dev/null) || cfg=
		[[ -n $cfg && $cfg != null ]] && configs+=("$cfg")
	fi
	# 2. Image config `created` (normal container images; Helm configs have none).
	if [[ -z $newest ]]; then
		for cfg in "${configs[@]}"; do
			[[ -z $cfg ]] && continue
			created=$(image_blob "$host" "$repo" "$cfg" 2>/dev/null | jq -r '.created // empty' 2>/dev/null) || created=
			[[ -z $created ]] && continue
			consider_created "$created"
			src=config
		done
	fi
	# 3. Registry publish time when the registry exposes one.
	if [[ -z $newest ]]; then
		created=$(image_registry_published "$host" "$repo" "$rest" 2>/dev/null) || created=
		[[ -n $created ]] && { consider_created "$created"; src=registry; }
	fi
	if [[ -z $newest ]]; then
		report "image $ref" unknown "$host: no created annotation, image config time or registry publish time"
	else
		report "image ${ref:0:70}" "$(age_days "$newest")" "$host ${src} $newest"
	fi
	return 0
}

# Helm chart dependencies: OCI repositories are read like images, HTTP repositories from index.yaml.
check_chart_dep() { # check_chart_dep <name> <version> <repository>
	local name=$1 ver=$2 repo=$3 t
	if [[ $repo == oci://* ]]; then
		check_image "${repo#oci://}/$name:$ver"
	else
		t=$(fetch "${repo%/}/index.yaml" | yq -r ".entries[\"$name\"][] | select(.version == \"$ver\") | .created" | head -1) || t=
		report "chart $name $ver" "$([[ -n $t && $t != null ]] && age_days "$t" || echo unknown)" "${repo} index.yaml ${t:-no answer}"
	fi
}

# --- diff scanning ------------------------------------------------------------------------------

# Dispatch --------------------------------------------------------------------------------------
case ${1:-} in
go|action|image|github|npm|url)
	kind=$1
	shift
	case $kind in
	go) check_go "$1" "$2" ;;
	action) check_action "$1" "$2" "${3:-}" ;;
	image) check_image "$1" ;;
	github) check_github "$1" "$2" ;;
	npm) check_npm "$1" "$2" ;;
	url) check_url "$1" ;;
	esac
	[[ $fails -eq 0 ]] || exit 1
	exit 0
	;;
esac

base=${1:-origin/main}
mergebase=$(git merge-base "$base" HEAD)

# added_lines <path-glob...>: "file<TAB>line" for every line the change adds.
# Every Dockerfile, including named ones such as hack/e2e-toolchain/web.Dockerfile and Dockerfile.mcp.
dockerfile_specs=('**/Dockerfile*' 'Dockerfile*' '**/*.Dockerfile' '*.Dockerfile')

added_lines() {
	git diff -U0 --no-color --diff-filter=AMR "$mergebase" HEAD -- "$@" |
		awk '/^\+\+\+ b\// { f = substr($0, 7); next } /^\+\+\+ / { next } /^\+/ { print f "\t" substr($0, 2) }'
}

echo "dependency age check (ADR 0079): minimum ${MIN_AGE_DAYS} days, changes since $(git rev-parse --short "$mergebase")"

seen=$'\n'
once() { # once <key>: true the first time a key is seen
	[[ $seen == *$'\n'"$1"$'\n'* ]] && return 1
	seen+="$1"$'\n'
}

# 1. Go modules and the toolchain.
while IFS=$'\t' read -r file line; do
	if [[ $line =~ ^go[[:space:]]+([0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$ ]]; then
		once "gotool ${BASH_REMATCH[1]}" && check_go_toolchain "${BASH_REMATCH[1]}"
	elif [[ $line =~ ^[[:space:]]*(require[[:space:]]+|tool[[:space:]]+)?([A-Za-z0-9._~/-]+\.[A-Za-z0-9._~/-]+)[[:space:]]+(v[0-9][^[:space:]]*) ]]; then
		once "go ${BASH_REMATCH[2]} ${BASH_REMATCH[3]}" && check_go "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
	fi
done < <(added_lines 'go.mod' '**/go.mod')

# 2. GitHub Actions pinned by SHA.
while IFS=$'\t' read -r file line; do
	if [[ $line =~ uses:[[:space:]]*([A-Za-z0-9._-]+/[A-Za-z0-9._/-]+)@([0-9a-f]{40})([[:space:]]*#[[:space:]]*([^[:space:]]+))? ]]; then
		spec=${BASH_REMATCH[1]}
		sha=${BASH_REMATCH[2]}
		tag=${BASH_REMATCH[4]:-}
		[[ $spec == ./* || $spec == docker://* ]] && continue
		once "action $spec $sha" && check_action "$spec" "$sha" "$tag"
	fi
done < <(added_lines '.github/workflows/*.yml' '.github/workflows/*.yaml' '.github/actions/**')

# 3. Images pinned by digest, in any file that is not documentation or a lockfile.
img_re='([a-z0-9][a-z0-9._-]*(:[0-9]+)?/)*[a-z0-9][a-z0-9._-]*(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}'
while IFS=$'\t' read -r file line; do
	[[ $line =~ ^[[:space:]]*#[[:space:]]*(checked|computed|sha256) ]] && continue
	rest=$line
	while [[ $rest =~ $img_re ]]; do
		ref=${BASH_REMATCH[0]}
		rest=${rest#*"$ref"}
		# `ADD --checksum=sha256:...` and `sha256:` alone are not images (no name before the @).
		[[ $ref == *@sha256:* && ${ref%%@*} != "" ]] || continue
		# docker/dockerfile:1.19@sha256 is a real image (the BuildKit frontend) and is checked too.
		once "image $ref" && check_image "$ref"
	done
done < <(added_lines . ':!*.md' ':!*.lock' ':!*pnpm-lock.yaml' ':!go.sum' ':!docs/' ':!**/testdata/**' ':!.github/dependabot.yml' ':!*.json')

# expand_vars <file> <url>: replaces ${NAME} and $NAME in a URL with the default the same file gives
# NAME (`ARG NAME=…`, `ENV NAME=…`, `NAME=…`, a YAML `NAME: …` or `: "${NAME:=…}"`), so a version kept in a variable is
# dated. A name the file does not define, or defines with two different values, is left as it is, and dating it then fails (fail closed).
expand_vars() {
	local file=$1 url=$2 name val
	while [[ $url =~ \$\{?([A-Za-z_][A-Za-z0-9_]*)\}? ]]; do
		name=${BASH_REMATCH[1]}
		val=$(sed -nE "s/^[[:space:]]*(ARG|ENV)?[[:space:]]*${name}=[\"']?([^\"' ]+)[\"']?.*/\\2/p; s/^[[:space:]]*(-[[:space:]]*)?${name}:[[:space:]]+[\"']?([^\"' #]+)[\"']?.*/\\2/p; s/.*\\\$\\{${name}:=([^}]+)\\}.*/\\1/p" "$file" 2>/dev/null | sort -u)
		# A workflow keeps its tool versions in hack/versions.env (loaded as ${{ env.NAME }}).
		if [[ -z $val && -f hack/versions.env ]]; then
			val=$(sed -nE "s/^${name}=([A-Za-z0-9._+-]+)$/\\1/p" hack/versions.env | sort -u)
		fi
		# Not defined, or defined with different values in one file (ambiguous): leave it, so dating fails.
		[[ -n $val && $val != *$'\n'* ]] || break
		url=${url//"\${$name}"/$val}
		url=${url//"\$$name"/$val}
	done
	printf '%s' "$url"
}

# 4. Release assets, raw tags and npm tarballs fetched by Dockerfiles and scripts.
while IFS=$'\t' read -r file line; do
	# A workflow's ${{ env.NAME }} reads like a shell ${NAME} (expand_vars finds it in hack/versions.env).
	line=$(sed -E 's/\$\{\{ *env\.([A-Za-z_][A-Za-z0-9_]*) *\}\}/${\1}/g' <<<"$line")
	# shellcheck disable=SC2013 # the matched urls contain no whitespace
	for url in $(grep -Eo 'https://(github\.com/[^/ ]+/[^/ ]+/releases/download/[^ ]+|raw\.githubusercontent\.com/[^ ]+|registry\.npmjs\.org/[^ ]+\.tgz)' <<<"$line" | sed 's/[\\"]*$//'); do
		url=$(expand_vars "$file" "$url")
		once "url $url" && check_url "$url"
	done
done < <(added_lines "${dockerfile_specs[@]}" 'hack/*.sh' 'test/e2e/**/*.sh' '.github/workflows/*.yml' '.github/workflows/*.yaml' ':!hack/pinned-download-lint.sh')

# 4a. The pnpm binary (ADR 0079 amendment, issue 450): `"packageManager": "pnpm@X"` in a package.json and the
# e2e toolchain's `ARG PNPM_VERSION=X` are npm releases of `pnpm`. pnpm's own bootConfig applies the cooldown when
# pnpm installs itself; this dates the pin at review time, so a hand-written bump is checked as well.
# pm_pin <line>: prints "<package> <version>" for a packageManager pin or the PNPM_VERSION default.
pm_pin() {
	local re_pm='"packageManager"[[:space:]]*:[[:space:]]*"([a-z]+)@([0-9][0-9A-Za-z.-]*)(\+[^"]*)?"'
	local re_arg='^[[:space:]]*ARG[[:space:]]+PNPM_VERSION=([0-9][0-9A-Za-z.-]*)[[:space:]]*$'
	if [[ $1 =~ $re_pm ]]; then
		echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"
	elif [[ $1 =~ $re_arg ]]; then
		echo "pnpm ${BASH_REMATCH[1]}"
	fi
}
while IFS=$'\t' read -r file line; do
	pin=$(pm_pin "$line")
	[[ -n $pin ]] || continue
	read -r pm_name pm_ver <<<"$pin"
	once "npm $pm_name $pm_ver" && check_npm "$pm_name" "$pm_ver"
done < <(added_lines '**/package.json' 'package.json' "${dockerfile_specs[@]}")

# 4b. The tool versions of hack/versions.env (old issue 544): the release each <TOOL>_VERSION names. The file
# is also what the workflows, Dockerfile and CONTRIBUTING copies are checked against
# (hack/versions-check.sh), so dating it dates the release assets they download.
# tool_repo <TOOL>: the GitHub repository of the release; an unknown name warns (like pin_repo).
tool_repo() {
	case $1 in
	KIND) echo "kubernetes-sigs/kind" ;;
	KO) echo "ko-build/ko" ;;
	HELM) echo "helm/helm" ;;
	KUBECTL) echo "kubernetes/kubernetes" ;;
	GITLEAKS) echo "gitleaks/gitleaks" ;;
	SHELLCHECK) echo "koalaman/shellcheck" ;;
	ACTIONLINT) echo "rhysd/actionlint" ;;
	GORELEASER) echo "goreleaser/goreleaser" ;;
	COSIGN) echo "sigstore/cosign" ;;
	KUBECONFORM) echo "yannh/kubeconform" ;;
	GOLANGCI_LINT) echo "golangci/golangci-lint" ;;
	BUF) echo "bufbuild/buf" ;;
	GOVULNCHECK) echo "golang/vuln" ;;
	GOFUMPT) echo "mvdan/gofumpt" ;;
	CRANE) echo "google/go-containerregistry" ;;
	esac
}
while IFS=$'\t' read -r file line; do
	if [[ $line =~ ^([A-Z][A-Z0-9_]*)_VERSION=([^[:space:]#]+) ]]; then
		tool=${BASH_REMATCH[1]}
		ver=${BASH_REMATCH[2]}
		repo=$(tool_repo "$tool")
		if [[ -n $repo ]]; then
			prefix=v
			[[ $ver == v* ]] && prefix=
			once "pin $repo $prefix$ver" && check_github "$repo" "$prefix$ver"
		else
			# Fail closed: a tool whose release cannot be dated would skip the cooldown (ADR 0079).
			report "$tool $ver (no entry in tool_repo of hack/dep-age-check.sh)" unknown "$file"
		fi
	fi
done < <(added_lines 'hack/versions.env')

# 5. ADR 0077 platform pins: the variables in test/e2e/kind/up.sh and marked lines anywhere.
# pin_repo <VARIABLE>: "<owner/repo> <tag prefix>" for the pins in test/e2e/kind/up.sh.
pin_repo() {
	case $1 in
	AS_VERSION) echo "kubernetes-sigs/agent-sandbox v" ;;
	OS_VERSION) echo "NVIDIA/OpenShell v" ;;
	KNP_VERSION) echo "kubernetes-sigs/kube-network-policies v" ;;
	CNPG_VERSION) echo "cloudnative-pg/cloudnative-pg v" ;;
	esac
}
while IFS=$'\t' read -r file line; do
	if [[ $file == test/e2e/kind/up.sh && $line =~ ^([A-Z_]+_VERSION)=([^[:space:]#]+) ]]; then
		var=${BASH_REMATCH[1]}
		ver=${BASH_REMATCH[2]}
		spec=$(pin_repo "$var")
		if [[ -n $spec ]]; then
			read -r repo prefix <<<"$spec"
			[[ $ver == v* ]] && prefix=
			once "pin $repo $prefix$ver" && check_github "$repo" "$prefix$ver"
		else
			echo "WARN    $file: $var has no entry in pin_repo (hack/dep-age-check.sh); its age is not checked"
		fi
	fi
done < <(added_lines 'test/e2e/kind/up.sh')
while IFS=$'\t' read -r file line; do
	if [[ $line =~ dep-age:[[:space:]]*github=([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)([[:space:]]+tag-prefix=([^[:space:]]*))? ]]; then
		repo=${BASH_REMATCH[1]}
		prefix=${BASH_REMATCH[3]:-}
		if [[ $line =~ [^0-9.]v?([0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+-]*) ]]; then
			once "pin $repo $prefix${BASH_REMATCH[1]}" && check_github "$repo" "$prefix${BASH_REMATCH[1]}"
		else
			echo "WARN    $file: dep-age marker without a version on the line: $line"
		fi
	fi
done < <(git diff -U0 --no-color --diff-filter=AMR "$mergebase" HEAD -- . ':!hack/dep-age-check.sh' ':!hack/pinned-download-lint.sh' ':!docs/' ':!*.md' |
	awk '/^\+\+\+ b\// { f = substr($0, 7); next } /^\+\+\+ / { next } /^\+/ { print f "\t" substr($0, 2) }')

# 6. Chart dependencies.
for chart in charts/*/Chart.yaml; do
	[[ -f $chart ]] || continue
	git diff --quiet "$mergebase" HEAD -- "$chart" && continue
	base_deps=$(git show "$mergebase:$chart" 2>/dev/null | yq -r '.dependencies[]? | .name + " " + .version + " " + .repository' 2>/dev/null || true)
	while read -r name ver repo; do
		[[ -z $name ]] && continue
		grep -qxF "$name $ver $repo" <<<"$base_deps" && continue
		once "chart $name $ver" && check_chart_dep "$name" "$ver" "$repo"
	done < <(yq -r '.dependencies[]? | .name + " " + .version + " " + .repository' "$chart")
done

echo
if ((fails > 0)); then
	echo "$fails of $checked checked versions are younger than ${MIN_AGE_DAYS} days (or could not be dated)."
	echo "Wait, or take the security fast path or a recorded waiver (ADR 0079, CONTRIBUTING.md)."
	exit 1
fi
if [[ -n $WAIVER ]]; then
	echo "$checked versions checked; young versions are WAIVED by ${WAIVER} (label dependency-age-waiver)."
else
	echo "$checked versions checked; none younger than ${MIN_AGE_DAYS} days."
fi
