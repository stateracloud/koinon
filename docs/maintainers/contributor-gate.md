<!-- SPDX-License-Identifier: Apache-2.0 -->

# The first-time contributor gate

For maintainers. A pull request whose author has no merged contribution in Koinon yet fails the
`contributor gate` check until a maintainer vouches for them, the way projects such as Ghostty gate first
pull requests. It is **off**: Koinon has no outside contributors yet. The plan is to turn it on with the first
public packages (review package 5D, #707). Issue: #848.

## What it does when on

`.github/workflows/contributor-gate.yml` runs `hack/contributor-gate.sh` on every pull request event that can
change the answer (opened, reopened, synchronize, labeled, unlabeled) and on every merge queue build. The author
passes when any of these holds:

| Rule | Source |
|---|---|
| GitHub's `author_association` is `OWNER`, `MEMBER`, `COLLABORATOR` or `CONTRIBUTOR` (a commit already merged) | the event |
| The login is a line of `.github/vouched.txt` | the base branch tip |
| The pull request has the `vouched` label | the event; in the queue, the pull request read with `gh api` (only triage rights or more can set a label) |

Anything else fails, an empty or unknown association included. GitHub reports a private organization
membership as `NONE` or `FIRST_TIME_CONTRIBUTOR` to this job, so a member who keeps it private and has no
merged commit yet needs a vouch (or a public membership). Both maintainers and every past contributor
pass on the first rule, so the gate never touches their pull requests. The failure message tells the author
how a vouch happens.

The job runs with a read-only token and no secret (no `pull_request_target`, rule W3 of
`docs/dev/ci-rules.md`). It checks out the base branch, never the pull request's code: on a pull request the
branch tip (`base.ref`, so a rerun sees a vouch merged since), on a merge queue build the queue's base, where it
reads the queued pull request's number from the queue ref and judges it again (`check-pr`, as claims.yml does). That
covers the whole build at the merge queue's group size of 1 (ADR 0094); a larger group needs the step to judge
every pull request in it first.
So the script and the vouch list are main's. The workflow file itself is the pull request's own, as on every
`pull_request` run: a pull request that edits it could turn its own check green. The guard against that is the
code-owner review of `.github/` (`.github/CODEOWNERS`), the same trade-off as the `claim` check. The gate posts
no comment and closes nothing; the red check is the whole effect.

While off, the job's `if:` starts with `false`, so no runner starts and nothing is checked. GitHub still lists
a skipped `contributor gate` check on each pull request (and runs the workflow, without a job, on each label
event); a skipped check blocks nothing and the repository's scripts count it as done.

## Vouch for someone

- One pull request: add the `vouched` label. The gate reruns on the label event and passes.
- For good: add the login to `.github/vouched.txt` with a comment that says who vouched and why. It takes
  effect once the line is on main, at the next run of the check (a rerun, a push or a label event: the gate
  reads the base branch tip). Their first merged pull request makes them a `CONTRIBUTOR`, after which no vouch
  is needed.
- Withdraw a vouch: remove the label (the gate reruns on the event) or the line (then rerun the check).

## Turn it on

Do these in one sitting; each needs a maintainer.

1. Create the label: `gh label create vouched --description "a maintainer vouched for the author (contributor gate)"`.
2. Check the bot and agent authors that open pull requests here (Dependabot, the Copilot coding agent, any
   App): one without a `CONTRIBUTOR` association fails the gate. `.github/vouched.txt` lists `dependabot[bot]`;
   add the others with a reason.
3. In `.github/workflows/contributor-gate.yml`, change the first word of the job's switch line,
   `if: false && (github.event_name == 'merge_group' || github.event_name == 'pull_request')`, to `true`.
   Nothing else on the line changes. `hack/contributor-gate-test.sh` accepts only these two forms and prints
   which one is set. This is a `.github/**` change, so it merges with `hack/pr-merge.sh <PR> --admin` (ADR 0105).
4. Make the check required, or it is only a red mark that does not block the merge:
   - add `contributor gate   .github/workflows/contributor-gate.yml` to the header of
     `hack/ci-required.txt` and `external contributor gate` to its list (in the same pull request as
     step 3). `hack/ci-required-test.sh` and `hack/ci-merge-group-test.sh` check the workflow, and
     `hack/contributor-gate-test.sh` already runs both on exactly this configuration in a scratch copy. Add
     `contributor-gate` to the list of workflows that produce a required context in `docs/dev/ci.md`
     ("Required status checks"). `hack/release-gate.sh` already treats `contributor gate` as a pull request
     check (it has no push run): it reads the merge queue build of the tagged tree, or the merged pull
     request's head after an admin merge;
   - after it merges, add `contributor gate` to the required status checks of the `main` ruleset and check
     with `hack/ci-required-test.sh --ruleset`. Bind it to the GitHub Actions app (rule Q5 of
     `docs/dev/ci-rules.md`). A ruleset change needs the owner's yes (`docs/dev/ci-rules.md`,
     "Stop and ask"). From then on a release tag needs a commit queued after the switch: an older commit's
     queue build has a skipped `contributor gate`, which `hack/release-gate.sh` refuses.
5. Add a line to the "Pull requests" section of `CONTRIBUTING.md`: a first pull request needs a maintainer's
   vouch, and how to ask for one.

Pull requests opened before the switch carry a skipped `contributor gate` check, which the ruleset counts as
passing; they are judged at their next push or label event, and in any case by the merge queue build.

GitHub's own setting "Require approval for workflows from outside collaborators" (Settings, Actions) still
applies: a first-time contributor's workflows wait until a maintainer approves the run. Approving the run is
not a vouch; the gate then runs and fails until the label or the line is there.

## Turn it off

1. Remove `contributor gate` from the required status checks of the `main` ruleset first (the owner's yes).
   Until step 2 merges, `hack/ci-required-test.sh --ruleset` reports the list entry missing from the ruleset,
   as expected. Doing it in this order keeps release tags working: a commit queued with the switch off has a
   skipped `contributor gate`, which `hack/release-gate.sh` refuses while the check is required.
2. In one pull request: set the switch back to `false`, and remove the two `contributor gate` lines from
   `hack/ci-required.txt` (`hack/ci-merge-group-test.sh` fails on a required job that a queue build skips).
   It merges with `hack/pr-merge.sh <PR> --admin` (a `.github/**` change, ADR 0105).
