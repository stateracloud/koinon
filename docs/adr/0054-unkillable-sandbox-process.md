# 0054. A sandbox with an unkillable process is recreated

- Status: Accepted; amended 2026-10-24 (below): detection, saving the work, the loop guard
- Date: 2026-10-23
- Amends: [0051](0051-session-home-generations.md) (adds a reason for recreating a sandbox)

## Context

A process in uninterruptible sleep (state D, usually stuck on storage or a network filesystem)
cannot be killed, even by `SIGKILL`. agentd's process sweep ([0051](0051-session-home-generations.md),
plan item AG1) then cannot end it, and the sweep fails the run. Left alone, every later run in that
sandbox fails the same way.

## Decision

1. When the sweep finds a process it cannot end because it is in D state, and the state lasts
   60 seconds, the sandbox is marked unhealthy.
2. koinon-reconciler recreates an unhealthy sandbox. The recreation is audited, with the process
   and the sandbox named.
3. The agent's home is kept through checkpoints when they exist ([0032](0032-agent-home-checkpoints.md)).
   Without checkpoints (v0.1), the home goes with the sandbox, as for any recreation, and the next
   run starts a new session ([0051](0051-session-home-generations.md)).

## Alternatives considered

- **Wait indefinitely** _(reconstructed)_. A D-state process can stay for hours; the agent is
  unusable meanwhile.
- **Reboot the node** _(reconstructed)_. Hits every tenant on the node for one agent's problem.
- **Fail the run and keep the sandbox** _(reconstructed)_. The next run hits the same process.

## Consequences

- Positive: a stuck sandbox heals without an operator.
- Negative: without checkpoints the agent loses its home, including conversation history, though
  tickets keep their messages.
- Negative: the stuck process may leave the underlying storage problem in place _(inferred)_.
- Positive: uncommitted work survives the recreation when the agent can reach the repository, and
  only with a human's approval.
- Negative: a file in a saved bundle can be a workflow (`.github/`), which a pushed branch may run
  if the repository has `on: push` workflows with secrets; the card says so. The push needs
  `contents: write` and GitHub may refuse workflow files.
- Negative: three recreations in a day stop the agent until an admin acts.

## Upstream verification

None needed.

## Links

- ADRs [0032](0032-agent-home-checkpoints.md), [0051](0051-session-home-generations.md)

## Amendment (2026-10-24): detection, saved work and a loop guard

Decided by the product owner. It settles the two questions this ADR left open: whether a recreation held off by a parked run is overridden (it is), and how a stuck process is detected (from `/proc`, inside the sandbox). Plan item AG1 builds it.

1. **Detection.** agentd's sweep ends the sandbox user's processes (TERM, then up to four KILL
   rounds, [0051](0051-session-home-generations.md)). If processes are still there after the last
   round, agentd re-reads `/proc` for them every `stuckPoll` (5 s). Any of them still present
   `stuckAfter` (60 s) after the sweep gave up counts as stuck, **whatever its state**: not only
   `D`. A zombie is the exception, as in the sweep: it is dead and only waits for its parent. For
   each stuck process agentd records pid, comm, state and wchan (`procinfo.sh`; the process start
   time guards against a reused PID) in `sandbox_incidents` and the audit log. The same applies to
   the sweep before a run: a run that finds a stuck sandbox waits for the recreated one. The 60 s
   are counted from the end of the sweep, which sends its last SIGKILL a few seconds earlier, so a
   survivor has outlived at least 60 s of SIGKILL.
2. **Save the work, best effort, 30 s.** Before anything is deleted, agentd reads the workspace's
   uncommitted work (`savework.sh`): in each git repository under the session directory whose
   origin is a GitHub repository, the working tree against the newest commit GitHub has (the parent
   of the oldest local-only commit, else `HEAD`), untracked files included, ignored files not,
   through a temporary index so the repository is not changed. Symbolic links, submodules and
   paths git has to quote are left out and reported (as lines the sandbox reports: at most 20 of
   200 bytes, shown as "reported by the sandbox, not verified"). At most 50 MB. The files are packed
   (gzip-compressed tar plus a manifest) and **posted by agentd to koinon-api**
   (`POST /internal/saved-work` on a listener of its own, `api.savedWorkPort`), which validates the bundle (paths, modes,
   sizes, the repository against the agent version's binding, the run still owning its sandbox),
   seals it with the envelope secret store ([0056](0056-model-providers-pricing-and-budgets-per-org.md))
   into the `saved_work` table, bound to the row, tied to the ticket and run, expiring after
   **14 days** (the approvals sweeper drops the sealed files and expires the request).
   **koinon-agentd holds no key-encryption key**: a compromised agentd cannot open any org's
   secrets. The listener is separate from the tools listener, which trusts agentgateway's agent
   header and so stays agentgateway's alone: the chart's network policy admits agentd to the
   upload port and only the gateway to the tools port; sandboxes reach neither. Each request
   carries an HMAC over org, run, epoch, expiry (at most 5 minutes ahead), repository number, the
   SHA-256 of the body and a random nonce. The key is a Kubernetes Secret (`<release>-saved-work`)
   that koinon-api creates on first use, create-if-absent and never overwritten, like the KEK
   Secret; agentd mounts that Secret only, and it is not a KEK. koinon-api remembers each nonce
   per run (`saved_work_upload_nonces`), so a captured request cannot be replayed, and refuses a
   body other than the one the MAC covers. The key is not in Postgres, so a database backup alone
   reveals nothing of it.
   Memory: agentd and koinon-api each handle one bundle at a time (a semaphore), because a
   bundle is up to 50 MB. **Why Postgres:** at most 50 MB for 14 days does not justify a second
   store; the cost is database size (a 50 MB value is about 67 MB of JSON, read only to push). The
   manifest (paths, modes, line counts) is stored in the clear for the approval card; it holds no
   content. The approval **binds to the content**: its arguments carry the SHA-256 of the packed
   bundle and of the manifest, and the push refuses, after opening, any bundle or manifest that
   does not match.
   The ticket timeline offers **"Push saved work to a branch"** as an ordinary ask-tier approval
   request ([0042](0042-ask-tier-park-and-execute.md)), made by the system: connector `github`,
   tool `push_saved_work`, `system` set so that the run neither parks on it nor is told its
   outcome, and it outlives the run. On approval the API pushes
   `koinon/wip/<ticket>-<time>` through the GitHub connection of the run's agent version (blobs, a
   tree on the recorded base commit, one commit with the usual trailers, a new branch), with the
   request ID as idempotency key. It never writes the default branch or an existing branch,
   refuses a repository the agent version is not bound to or the installation cannot reach, and
   fails if the base commit is not on GitHub. Nothing reaches GitHub without an approval. If saving
   fails or runs out of time, the timeline says the uncommitted work was lost and why.
3. **Recreate.** agentd marks the sandbox unhealthy (`agent_sandboxes.unhealthy_*`, with the home
   generation it was found in) and wakes the reconciler. The reconciler deletes that sandbox at
   once, **overriding the hold of parked runs** (answering the first open question), and creates a
   new one; the new home generation ends the mark. The recreation is audited
   (`sandbox.mark_unhealthy`, `sandbox.recreate`) and the ticket gets "Sandbox recreated: <reason>".
   A parked run stays parked in Postgres and resumes in the new sandbox through the reset path of
   [0051](0051-session-home-generations.md). Sandbox deletion can itself take long when the node's
   storage is what hangs; agentd's wait for a ready sandbox is bounded as ever.
4. **Loop guard.** At most three recreations per agent in 24 hours. The next time a sandbox is found
   stuck, it is **not** recreated: the agent is **unhealthy** (`agents.unhealthy_at`). New runs are
   refused with the reason (ticket creation and messages). Runs already queued, and parked runs whose approval is decided, **wait** (`ClaimNextRun` skips an unhealthy agent) and start once the agent is resumed; every ticket with such a run gets a note saying why, and its page shows the agent's state with a link to Resume. Owners and admins
   get an in-app notification (the notification store of the budgets, kind `agent.unhealthy`), and
   an admin's **Resume agent** (UI and `AgentService.ResumeAgent`, audited) clears it, marks the
   stuck sandbox for recreation and starts a new count.

What cannot be tested end to end: a process in uninterruptible sleep cannot be made on kind. The
detection, the saving, the recreation and the loop guard are tested against a fake `/proc` and a
fake sandbox that cannot end a process; only the shell scripts run against real git and a real
`/proc` layout.

## Amendment (2026-10-27, HC1 review): agentd still holds no key-encryption key with home checkpoints

Home checkpoints (ADR 0032) make koinon-agentd the writer of agents' homes, which needs a password
per agent repository. That password is sealed with the secret store, so the rule above holds: agentd
does not mount the KEK Secret. koinon-api opens (or creates) the password and returns it to agentd
per checkpoint, on the same listener and with the same kind of HMAC credential as saved work
(`/internal/home-repository`).

## Amendment (2026-10-30, old issue 260): savework reads the repositories with an isolated git config

The repositories are the agent's, and savework runs outside any turn, so it must not run code the
agent's git configuration names. `savework.sh` ignores the system, global and home configuration
(`GIT_CONFIG_NOSYSTEM`, `GIT_CONFIG_GLOBAL=/dev/null`; this includes the home include file that
`/etc/gitconfig` names, which the agent can write), forces `core.fsmonitor` and `core.hooksPath`
off, passes `--no-ext-diff --no-textconv`, empties every filter driver the repository defines,
so files are saved as they are on disk, and allows no transport (`GIT_ALLOW_PROTOCOL`), so a
partial clone cannot fetch a missing object from a promisor remote whose `uploadpack`,
`core.sshCommand` or `ext::` URL the agent chose. A repository whose own configuration includes other files
(`include`, `includeIf`) is skipped and reported, because git cannot be told to ignore includes in
the repository's config. Global and home ignore and attributes files (`core.excludesFile`, `core.attributesFile` and their XDG defaults) no longer apply to what is saved; only the repository's own `.gitignore` and `.gitattributes` do.

Permission requests (ADR 0049): agentd never picks an "always" option (a request with no once option
is cancelled), and the paths of a request are resolved in the sandbox, so a link inside the
workspace that points outside is outside; a path that is itself a link is outside.
