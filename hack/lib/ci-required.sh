#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Where the required checks of a commit come from (#971). hack/ci-required.txt is the source of truth and the
# ruleset mirrors it, so while a required check is renamed the two disagree for a while (docs/dev/ci.md "Renaming
# a required check"). A script that judges a commit reads the list of THAT commit, never the checkout it runs in
# and never today's ruleset. Source this file, do not run it.

# cir_parse: ci-required.txt on stdin -> the required contexts, one per line (comments, `advisory` rows and the
# `aggregator`/`external` prefixes dropped).
cir_parse() {
  sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' |
    awk 'NF && $1 != "advisory" { sub(/^(aggregator|external) /, ""); print }'
}

# cir_fetch <ref>: the raw hack/ci-required.txt at a commit or branch of the repository, from the API (the
# remote's state: a stale local checkout is never read). Fails when it cannot be read.
cir_fetch() {
  gh api -H 'Accept: application/vnd.github.raw' "repos/{owner}/{repo}/contents/hack/ci-required.txt?ref=$1"
}
