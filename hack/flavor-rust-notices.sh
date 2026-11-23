#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Generates harnesses/opencode/doc/THIRD_PARTY_NOTICES.md (#63): the crate dependency notices for
# the Rust tools the OpenCode flavor ships (ripgrep, fd, ast-grep). Each tool's own licence ships
# separately in the image (the Dockerfile's `licenses` and `tools` stages); this covers what each
# tool's Cargo.lock pulls in, with cargo-about (pinned in hack/versions.env), against
# harnesses/opencode/about.toml and about.hbs.
#
# Usage: hack/flavor-rust-notices.sh
#
# What it does, for each (crate, version) pair below:
#   1. Fetches the published crate (static.crates.io/crates/<crate>/<crate>-<version>.crate).
#   2. Runs `cargo about generate --fail --all-features -c about.toml -m <crate>/Cargo.toml about.hbs`
#      against the tool's own Cargo.lock (never regenerated: --locked), so the notice matches the
#      exact dependency versions the release binary was built from.
#   3. Concatenates the three outputs into harnesses/opencode/doc/THIRD_PARTY_NOTICES.md with a
#      per-tool header naming the tool and the pinned version. The Dockerfile copies this file
#      and its build-time check needs all three tools in it; test/harnesses compares the
#      versions with the Dockerfile pins (see harnesses/opencode/README.md).
#
# Needs network access to github.com (the cargo-about release) and to crates.io (the crates and the
# registry endpoints cargo-about reads each dependency's licence from); it downloads and
# executes the cargo-about binary. Run it on a machine that allows both, commit the result; the
# Dockerfile already copies it. See harnesses/opencode/README.md, "Rust tool dependency notices".
#
# Re-pinning a tool: update its (crate, version) below and in the Dockerfile together, rerun
# this script, and commit the new THIRD_PARTY_NOTICES.md alongside the Dockerfile change.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=/dev/null
. hack/versions.env

harness_dir=harnesses/opencode
out=$harness_dir/doc/THIRD_PARTY_NOTICES.md

# crates.io crate, its version, the tool name and version string for the notice header, and the
# path to the crate's Cargo.toml inside the extracted .crate. The published crate of a release
# carries the Cargo.lock the release was built from, and crates.io serves it without GitHub's
# archive endpoint (which some networks block). The .crate top directory is "<crate>-<version>".
tools=(
	"ripgrep 15.2.0 ripgrep 15.2.0 ."
	"fd-find 10.5.0 fd 10.5.0 ."
	"ast-grep 0.51.0 ast-grep 0.51.0 ."
)

work=$(mktemp -d "${TMPDIR:-/tmp}/flavor-rust-notices.XXXXXX")
trap 'rm -rf "$work"' EXIT

# The cargo-about build for this host. Linux gets the static musl build, macOS the Apple-silicon
# build; any other host stops here with a clear message instead of an "Exec format error" later.
case "$(uname -s)/$(uname -m)" in
Linux/x86_64) cargo_about_sha256=$CARGO_ABOUT_SHA256_AMD64 cargo_about_target=x86_64-unknown-linux-musl ;;
Linux/aarch64 | Linux/arm64) cargo_about_sha256=$CARGO_ABOUT_SHA256_ARM64 cargo_about_target=aarch64-unknown-linux-musl ;;
Darwin/arm64) cargo_about_sha256=$CARGO_ABOUT_SHA256_DARWIN_ARM64 cargo_about_target=aarch64-apple-darwin ;;
*)
	echo "flavor-rust-notices: unsupported host $(uname -s)/$(uname -m): cargo-about ${CARGO_ABOUT_VERSION} has builds for Linux (x86_64, aarch64) and macOS (arm64) only" >&2
	exit 1
	;;
esac

cargo_about_tgz=$work/cargo-about.tar.gz
hack/fetch-pinned.sh \
	"https://github.com/EmbarkStudios/cargo-about/releases/download/${CARGO_ABOUT_VERSION}/cargo-about-${CARGO_ABOUT_VERSION}-${cargo_about_target}.tar.gz" \
	"$cargo_about_sha256" "$cargo_about_tgz"
tar -xzf "$cargo_about_tgz" -C "$work" --strip-components=1 "cargo-about-${CARGO_ABOUT_VERSION}-${cargo_about_target}/cargo-about"
cargo_about=$work/cargo-about
chmod 0755 "$cargo_about"

# lock_version LOCKFILE CRATE: the version of CRATE in a Cargo.lock, empty when it is not there.
lock_version() {
	awk -v c="$2" '$1 == "name" && $3 == "\"" c "\"" { f = 1; next } f && $1 == "version" { gsub(/"/, "", $3); print $3; exit }' "$1"
}

# crate_file CRATE VERSION PATH: print PATH from the published crate CRATE-VERSION. The C libraries
# below are vendored in their -sys crates, which cargo-about does not read for a licence; the
# licence texts come from the vendored sources, never by hand.
crate_file() {
	local crate=$1 version=$2 path=$3 dir
	dir=$work/cfile-$crate
	if [ ! -d "$dir" ]; then
		mkdir -p "$dir"
		curl -fsSL --retry 5 --retry-all-errors "https://static.crates.io/crates/$crate/$crate-${version//+/%2B}.crate" | tar -xzf - -C "$dir" --strip-components=1
	fi
	cat "$dir/$path"
}

# licence_head: read a source file on stdin, print its head up to the end of the comment that holds
# the licence text (the first line ending in "*/" after "Redistribution and use"), and fail when
# the file has no such text.
licence_head() {
	local text
	text=$(awk '!done { print } /Redistribution and use/ { seen = 1 } seen && /\*\/[[:space:]]*$/ { done = 1 }')
	case $text in
	*"Redistribution and use in source and binary forms"*) printf '%s\n' "$text" ;;
	*) echo "flavor-rust-notices: no licence text in the first comment block of a vendored C source" >&2; return 1 ;;
	esac
}

# c_libraries LOCKFILE TOOL: the licence texts of the C libraries that the -sys crates in the lock
# vendor and link statically (PCRE2 and sljit through pcre2-sys, jemalloc through tikv-jemalloc-sys).
c_libraries() {
	local lock=$1 tool=$2 v
	v=$(lock_version "$lock" pcre2-sys)
	if [ -n "$v" ]; then
		echo
		echo "### C library in $tool: PCRE2 and sljit (vendored by pcre2-sys $v)"
		echo
		echo "Source: https://crates.io/crates/pcre2-sys/$v. The text below is the licence block at the top of the"
		echo "vendored source file; the crate has no separate licence file for these libraries."
		echo
		echo "PCRE2 (upstream/include/pcre2.h):"
		echo
		crate_file pcre2-sys "$v" upstream/include/pcre2.h | licence_head
		echo
		echo "sljit (upstream/deps/sljit/sljit_src/sljitLir.h):"
		echo
		crate_file pcre2-sys "$v" upstream/deps/sljit/sljit_src/sljitLir.h | licence_head
	fi
	v=$(lock_version "$lock" tikv-jemalloc-sys)
	if [ -n "$v" ]; then
		echo
		echo "### C library in $tool: jemalloc (vendored by tikv-jemalloc-sys $v)"
		echo
		echo "Source: https://crates.io/crates/tikv-jemalloc-sys/${v//+/%2B}. Text of jemalloc/COPYING:"
		echo
		crate_file tikv-jemalloc-sys "$v" jemalloc/COPYING
	fi
}

tmp=$work/THIRD_PARTY_NOTICES.md
: >"$tmp"
{
	echo "# Third-party notices, Rust tools (OpenCode flavor)"
	echo
	echo "Generated by hack/flavor-rust-notices.sh with cargo-about ${CARGO_ABOUT_VERSION}."
	echo "Do not hand-edit: rerun the script after re-pinning any of the tools below."
	echo
} >>"$tmp"

for entry in "${tools[@]}"; do
	read -r crate crate_version name _ subdir <<<"$entry"
	version=$crate_version
	src=$work/$name
	archive=$work/$name.tar.gz
	curl -fsSL --retry 5 --retry-all-errors -o "$archive" "https://static.crates.io/crates/$crate/$crate-$crate_version.crate"
	mkdir -p "$src"
	tar -xzf "$archive" -C "$src" --strip-components=1
	echo "flavor-rust-notices: generating notices for $name $version (crates.io $crate $crate_version)"
	{
		echo
		echo "## $name $version"
		echo
	} >>"$tmp"
	"$cargo_about" generate --fail --locked --all-features \
		-c "$(cd "$harness_dir" && pwd)/about.toml" \
		-m "$src/$subdir/Cargo.toml" \
		"$(cd "$harness_dir" && pwd)/about.hbs" >>"$tmp"
	c_libraries "$src/Cargo.lock" "$name" >>"$tmp"
done

mkdir -p "$(dirname "$out")"
mv "$tmp" "$out"
echo "flavor-rust-notices: wrote $out"
