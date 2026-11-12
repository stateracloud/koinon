<!-- SPDX-License-Identifier: Apache-2.0 -->

# The first-time contributor gate

For maintainers. A pull request whose author has no merged contribution in Koinon yet fails the
`contributor gate` check until a maintainer vouches for them, the way projects such as Ghostty gate first
pull requests. It is **off**: Koinon has no outside contributors yet. The plan is to turn it on with the first
public packages (review package 5D, #707). Issue: #848.

## What it does when on

`.github/workflows/contributor-gate.yml` runs `hack/contributor-gate.sh check` on every pull request event
that can change the answer (opened, reopened, synchronize, labeled, unlabeled). The author passes when any of
these holds:

| Rule | Source |
|---|---|
| GitHub's `author_association` is `OWNER`, `MEMBER`, `COLLABORATOR` or `CONTRIBUTOR` (a commit already merged) | the event |
| The login is a line of `.github/vouched.txt` | the base commit |
| The pull request has the `vouched` label | the event (only triage rights or more can set a label) |

Anything else fails, an empty or unknown association included. Both maintainers and every past contributor
pass on the first rule, so the gate never touches their pull requests. The failure message tells the author
how a vouch happens.

The job checks out the pull request's **base** commit with a read-only token and no secret, so the script
and the vouch list that judge a pull request are main's, not the pull request's own (no
`pull_request_target`, rule W3 of `docs/dev/ci-rules.md`). It posts no comment and closes nothing; the red
check is the whole effect. On a merge queue build the job is skipped, which reports success: the author of
a pull request never changes, so the pull request's own run already decided.

While off, the job's `if:` is `false`, so GitHub lists the check as skipped and starts no runner.

## Vouch for someone

- One pull request: add the `vouched` label. The gate reruns on the label event and passes.
- For good: add the login to `.github/vouched.txt` with a comment that says who vouched and why. It takes
  effect for pull requests opened against main after the line merges (the gate reads the base commit).
  Their first merged pull request makes them a `CONTRIBUTOR`, after which no vouch is needed.
- Withdraw a vouch: remove the label or the line, then rerun the check.

## Turn it on

Do these in one sitting; each needs a maintainer.

1. Create the label: `gh label create vouched --description "a maintainer vouched for the author (contributor gate)"`.
2. In `.github/workflows/contributor-gate.yml`, change the job's switch line from
   `if: false && github.event_name == 'pull_request'` to `if: true && github.event_name == 'pull_request'`.
   `hack/contributor-gate-test.sh` accepts only these two forms and prints which one is set. This is a
   `.github/**` change, so it merges with `hack/pr-merge.sh <PR> --admin` (ADR 0105).
3. Make the check required, or it is only a red mark that does not block the merge:
   - add `contributor gate   .github/workflows/contributor-gate.yml` to the header of
     `hack/ci-required.txt` and `external contributor gate` to its list (in the same pull request as
     step 2; `hack/ci-required-test.sh` checks that the workflow listens to `merge_group`);
   - after it merges, add `contributor gate` to the required status checks of the `main` ruleset and check
     with `hack/ci-required-test.sh --ruleset`. A ruleset change needs the owner's yes (`docs/dev/ci-rules.md`,
     "Stop and ask").
4. Add a line to the "Pull requests" section of `CONTRIBUTING.md`: a first pull request needs a maintainer's
   vouch, and how to ask for one.

GitHub's own setting "Require approval for workflows from outside collaborators" (Settings, Actions) still
applies: a first-time contributor's workflows wait until a maintainer approves the run. Approving the run is
not a vouch; the gate then runs and fails until the label or the line is there.

## Turn it off

Reverse step 2 (`true` back to `false`). A required check whose job is skipped reports success, so nothing
waits on it; still remove it from the ruleset and from `hack/ci-required.txt` if it stays off for long, so the
required list names only checks that decide something.
