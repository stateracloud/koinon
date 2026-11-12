#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): part of the maintainers' merge loop. It only reads, so it works from a fork.
#
# List the unresolved review threads of a pull request (#187). The main ruleset requires every review
# thread resolved (required_review_thread_resolution), and advisory bots (DeepSource, CodeRabbit) leave
# inline comments, so a PR can sit BLOCKED with every required check green. hack/pr-wait.sh --checks and
# hack/pr-merge.sh call this on mergeStateStatus BLOCKED. It only reads: it never replies to or resolves a
# thread (a human or the main session does that, docs/dev/ci.md "Merging").
#
# Usage: hack/pr-threads.sh <PR>
# Output: one line per unresolved thread, `UNRESOLVED <path>:<line> <author> (outdated)`; the line is the
#         thread's current line, else the original one (`?` when the thread has neither, e.g. a file-level
#         comment); `(outdated)` is added when the diff moved on; the author is the first comment's.
#         Also each thread's id (`id=PRRT_...`), which resolveReviewThread takes.
# Exit: 0 no unresolved thread, 7 at least one (UNRESOLVED-THREADS, the same code in pr-wait.sh and
#       pr-merge.sh), 2 the threads could not be read, 64 usage.
# GraphQL reviewThreads, 50 per page, paginated with gh api --paginate (pageInfo.endCursor).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

[ $# -eq 1 ] || { echo "usage: hack/pr-threads.sh <PR>" >&2; exit 64; }
pr=$1
case $pr in '' | *[!0-9]*) echo "usage: hack/pr-threads.sh <PR>" >&2; exit 64 ;; esac

# shellcheck disable=SC2016 # $owner, $repo, $number, $endCursor are GraphQL variables
query='query($owner: String!, $repo: String!, $number: Int!, $endCursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 50, after: $endCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          originalLine
          comments(first: 1) { nodes { author { login } } }
        }
      }
    }
  }
}'

# One JSON document per page. The stream goes through jq here, not gh's --jq, which would run per page too
# but could not be tested without a real gh.
# The read is retried 3 times with backoff 2, 5, 15 s (PR_THREADS_RETRY_SLEEPS="0 0 0" in tests): callers
# treat exit 2 as "do not merge", so one transient gh failure must not end there.
read -r -a sleeps <<<"${PR_THREADS_RETRY_SLEEPS:-2 5 15}"
pages=""
ok=0
for ((try = 0; try <= ${#sleeps[@]}; try++)); do
  if pages=$(gh api graphql --paginate -F owner='{owner}' -F repo='{repo}' -F number="$pr" -f query="$query"); then
    ok=1
    break
  fi
  [ "$try" -lt "${#sleeps[@]}" ] && sleep "${sleeps[$try]}"
done
[ "$ok" = 1 ] || { echo "pr-threads: cannot read the review threads of PR $pr" >&2; exit 2; }

# A missing pull request is not "no threads".
jq -e -s 'all(.[]; .data.repository.pullRequest != null)' <<<"$pages" >/dev/null 2>&1 ||
  { echo "pr-threads: PR $pr not found (pullRequest is null in the answer)" >&2; exit 2; }

rows=$(jq -r '
  (.data.repository.pullRequest.reviewThreads.nodes // [])[]
  | select(.isResolved | not)
  | "UNRESOLVED \(.path // "?"):\(.line // .originalLine // "?") \(.comments.nodes[0].author.login // "ghost")\(if .isOutdated then " (outdated)" else "" end) id=\(.id)"
' <<<"$pages") || { echo "pr-threads: cannot parse the review threads of PR $pr" >&2; exit 2; }

[ -n "$rows" ] || exit 0
echo "$rows"
exit 7
