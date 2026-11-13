#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Scripted answer to one pending ask_user question (#967): sign in as an org member or above through /auth/login,
# find the open question by its run and question ID, and answer it with text taken from the run plan. For the
# real-model runs (the lab runs of #51, RT3 in docs/dev/real-model-tests.md) that need a question answered
# without a person at the inbox. The answer never comes from the model: the script sends the choice or text
# the plan holds, and only to the one question it names.
#
# Usage: hack/answer-question.sh <base-url> <cookie-jar> <org-id> <run-id> <question-id>
#   base-url, cookie-jar, org-id   as in hack/approve-pr.sh
#   run-id, question-id            the question must be open, belong to that run and be addressed to the approver
#                                  question-id may be the word `latest`: an unattended run does not know the ID, so
#                                  the script takes the one open question of that run, and refuses (exit 3, nothing
#                                  sent) when the run has none (after the wait) or more than one
# The answer comes from the environment, exactly one of:
#   KOINON_ANSWER_CHOICE  one of the question's choices, verbatim (the script refuses any other text)
#   KOINON_ANSWER_TEXT    free text
# The login (KOINON_APPROVER_USER, KOINON_APPROVER_PASSWORD) and KOINON_APPROVER_WAIT work as in
# hack/approve-pr.sh. The question list is read as one page.
# Exit: 0 answered, 1 an API refusal or an answer that is not one of the choices, 2 usage, 3 no such open question.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: ${0##*/} <base-url> <cookie-jar> <org-id> <run-id> <question-id>" >&2
  exit 2
fi
# shellcheck disable=SC2034 # base and jar are read by hack/lib/approver-session.sh
base=${1%/} jar=$2 org=$3 run=$4 qid=$5
choice=${KOINON_ANSWER_CHOICE:-} text=${KOINON_ANSWER_TEXT:-}
if [[ -n $choice && -n $text ]] || [[ -z $choice && -z $text ]]; then
  echo "set exactly one of KOINON_ANSWER_CHOICE and KOINON_ANSWER_TEXT" >&2
  exit 2
fi

# shellcheck source=/dev/null
source "$(dirname "${BASH_SOURCE[0]}")/lib/approver-session.sh"
approver_check_env
wait_s=$(approver_wait_seconds)
approver_login

svc=koinon.question.v1alpha1.QuestionService
deadline=$((SECONDS + wait_s))
while :; do
  list=$(rpc "$svc" ListQuestions "$(jq -n --arg o "$org" '{orgId: $o}')") || exit 1
  if [[ $qid == latest ]]; then
    n=$(jq --arg r "$run" '[.questions[]? | select(.runId == $r and .state == "QUESTION_STATE_OPEN")] | length' <<<"$list")
    if ((n > 1)); then
      echo "run $run has $n open questions in org $org; name one by its ID instead of latest" >&2
      exit 3
    fi
    q=$(jq -c --arg r "$run" '[.questions[]? | select(.runId == $r and .state == "QUESTION_STATE_OPEN")] | first // empty' <<<"$list")
  else
    q=$(jq -c --arg id "$qid" --arg r "$run" '[.questions[]? | select(.id == $id and .runId == $r and .state == "QUESTION_STATE_OPEN")] | first // empty' <<<"$list")
  fi
  [[ -n $q ]] && break
  if ((SECONDS >= deadline)); then
    echo "no open question $qid of run $run in org $org" >&2
    exit 3
  fi
  sleep 3
done

qid=$(jq -r '.id' <<<"$q")
if [[ -n $choice ]]; then
  jq -e --arg c "$choice" '.choices // [] | index($c) != null' <<<"$q" >/dev/null ||
    { echo "question $qid has no such choice; its choices are: $(jq -c '.choices // []' <<<"$q")" >&2; exit 1; }
  body=$(jq -n --arg o "$org" --arg q "$qid" --arg a "$choice" '{orgId: $o, questionId: $q, choice: $a}')
else
  body=$(jq -n --arg o "$org" --arg q "$qid" --arg a "$text" '{orgId: $o, questionId: $q, text: $a}')
fi
done_=$(rpc "$svc" AnswerQuestion "$body") || exit 1
echo "answered $qid of run $run: $(jq -r '.question.state // "?"' <<<"$done_")"
