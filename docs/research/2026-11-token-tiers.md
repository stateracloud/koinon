# Where agent tokens go: search and read tiers

- Status: Complete
- Date: 2026-11-02
- Area: engineering-process
- Tags: tooling

Research date: 2026-11-02, for #526. Source: the Claude Code session transcripts of this repository
(`~/.claude/projects/-Users-user-Developer-GitRepos-koinon/*.jsonl` and their
`subagents/**`). Read-only analysis; no task was replayed. Characters are the length of the tool
result text as stored in the transcript, which is what the model read. At roughly 3 to 4 characters
per token, 10k characters is about 3k tokens.

## Short answer

- **Most of the output is file reading done through `Bash`, not the Read tool.** `sed -n` and
  `cat` of files return 43M characters, 43% of all Bash output, against 10.8M for the Read tool.
- **The worst habit is reading several files or ranges in one call.** 2,928 subagent calls ran
  several `sed -n` or `cat` reads at once and returned 22.9M characters: 20% of all subagent tool
  output. They are still 14% after the subagent brief landed (2026-10-31).
- **The Read tool's 47k tail is mostly Bash output read a second time.** When a Bash result is too
  large, Claude Code saves it under `tool-results/` and shows about 2k characters. Agents then Read
  that file whole: 121 times, 5.2M characters, 49% of all Read characters.
- **The suspected culprits are small.** `go test` and ginkgo are 1.9% and 1.1% of Bash output (median
  about 450 characters), `gh run view --log` is 0.3%, kubectl 0.8%, make 0.5%. Agents already pipe
  them through `tail` or `grep`.
- **codegraph_explore is capped by characters, not by files.** Every answer is about 25k characters
  whether `maxFiles` is 3 or 12. A lower default changes nothing unless it is 1 or 2, and the tool
  is used about 3 times a day, so it is not where tokens go.

## Method and window

- Window: 2026-10-21 08:30 to 2026-11-02 12:58 UTC. 997 transcripts: 15 main sessions, 982
  subagents. 47,992 tool calls paired with their results (the issue's 57k also counts calls without
  a stored result).
- One pass extracts each call (tool, input, result length, error flag, timestamp, position in the
  transcript). Bash commands are put into families by their first real command, after dropping
  `cd`, `export`, bare variable assignments and leading `VAR=value` prefixes.
- "Before" and "after" split at 2026-10-31 12:00, when `docs/agents/subagent-brief.md` started to be
  handed to subagents. The two windows have different task mixes (the later one has more small
  workflow subagents), so the drop between them is not all the brief's doing.
- Savings are estimated as the characters a result had above 6,000 (about 120 lines of Go, the
  brief's read size). That assumes the agent needed the first part of the output and would not have
  made another call for the rest; it is an upper bound per call and a rough one per session.
- Scripts and intermediate data stayed in the session scratchpad; they are not committed.

## 1. Bash output by command family

38,189 Bash calls returned 100.1M characters. 2,200 of them (6%) returned 10k characters or more and
make up 35% of the total. The Bash tool truncates at about 30k; 277 outputs went over and were saved
to a file instead (see section 2).

| Family | Calls | Characters | Share | Median | p95 | Cut with |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| `sed -n` (file ranges) | 5,167 | 25.0M | 25.0% | 3,713 | 12,483 | one file, one range of ≤120 lines per call |
| `cat` | 3,548 | 19.3M | 19.2% | 2,170 | 20,287 | Read with offset+limit; never several files |
| `grep` | 6,813 | 15.9M | 15.9% | 1,481 | 7,378 | already filtered (92% piped through head, tail or grep) |
| `git diff` | 892 | 6.5M | 6.5% | 4,479 | 23,300 | `--stat` first, one path, `\| head` |
| `python` | 3,642 | 4.1M | 4.1% | 403 | 4,480 | nothing: mostly analysis scripts |
| `ls` | 1,410 | 3.7M | 3.7% | 1,261 | 9,369 | `/bin/ls` of one directory, not `-R` |
| `git show` | 679 | 3.6M | 3.6% | 3,344 | 16,182 | `git show REF:path \| sed -n` |
| loops (`for`, `while`) | 1,220 | 2.7M | 2.7% | 729 | 10,235 | one file per call |
| `wc` | 252 | 1.2M | 1.2% | 2,671 | 16,664 | `wc -l f && cat f` still prints the file |
| `git log` | 430 | 1.2M | 1.2% | 1,544 | 10,829 | `--oneline -n 20` |
| `git status` | 501 | 1.2M | 1.2% | 769 | 8,480 | `--short` |
| `sed` (not `-n`) | 1,106 | 1.1M | 1.1% | 420 | 4,102 | — |
| `git grep` | 404 | 1.0M | 1.0% | 1,717 | 6,882 | — |
| `gh api` | 433 | 0.9M | 0.9% | 1,074 | 7,031 | `--jq` |
| `gh issue view` | 402 | 0.9M | 0.9% | 1,477 | 5,871 | `--json body --jq` |

Commands counted anywhere in the command line, not only as the first one:

| Command | Calls | Share of Bash | Median | p95 | Note |
| --- | ---: | ---: | ---: | ---: | --- |
| `go test` | 2,000 | 1.9% | 452 | 3,947 | 89% already piped through tail/grep |
| ginkgo | 1,021 | 1.1% | 367 | 4,615 | |
| `gh run view --log` | 144 | 0.3% | 973 | 6,853 | `hack/ci-fail.sh`: 62 calls, median 570 |
| `hack/preflight.sh` | 533 | 0.8% | 680 | 5,155 | one line per gate works |
| kubectl / helm | 568 / 697 | 0.8% / 1.3% | 596 / 857 | 4,764 / 6,380 | |
| curl | 502 | 0.9% | 796 | 6,337 | |
| docker | 1,280 | 1.7% | 564 | 4,925 | |

### File reads through Bash

| Shape (subagents, whole window) | Calls | Characters | Share of all subagent output | Above 6k |
| --- | ---: | ---: | ---: | ---: |
| several `sed -n` or `cat` files in one call | 2,928 | 22.9M | 20.0% | 9.0M |
| one `sed -n` range of more than 120 lines | 612 | 5.2M | 4.5% | 2.0M |
| `cat` of one whole file | 877 | 6.4M | 5.6% | 2.7M |
| one `sed -n` range of at most 120 lines | 2,277 | 6.9M | 6.0% | 0.6M |
| `git diff` / `git show` of content | 1,141 | 8.6M | 7.6% | 3.9M |
| `git diff` / `git show` with `--stat` or `--name-only` | 311 | 1.2M | 1.0% | 0.3M |

`cat` reads by file type: Go 7.1M, Markdown 3.8M, YAML 1.1M, TSX 0.8M, SQL 0.7M.

## 2. Read calls without offset or limit

| | Calls | Characters | Share of Read characters | Median | p95 |
| --- | ---: | ---: | ---: | ---: | ---: |
| all Read calls | 929 | 10.8M | 100% | 1,336 | 48,655 |
| without offset/limit | 701 | 8.7M | 80.8% | 783 | 50,857 |
| with offset or limit | 228 | 2.1M | 19.2% | 2,492 | 34,846 |

- 181 of the 226 Reads of 20k characters or more had no offset or limit.
- Of the 701 whole Reads, **28 were followed by an Edit or Write of the same file** (365k
  characters). 673 never were (8.4M); 215 of those were 10k or more (7.9M).
- What the large whole Reads were: saved Bash or tool output under `tool-results/` (105 calls,
  4.5M), Markdown (158 calls, 3.0M, mostly research reports and retro reports in a scratchpad that a
  reviewer had to read), Go source only 17 calls (0.3M).
- The saved-output chain: 277 Bash outputs went over the limit (110 were `cat`, 85 `git diff` of a
  whole branch). 121 of them were then Read in full, 5.2M characters. The 2k preview had already
  been paid for, so these outputs cost twice. After 2026-10-31 this is still 3.9k characters per
  subagent transcript.
- Edit: 40 of 950 calls failed (4.2%), 35 of them "file not read first". The brief already has the
  rule; the remaining failures are agents that read with `sed` and then edit.

So the Read tail is not caused by agents reading source files whole. It is caused by large Bash
output read a second time, and by reviewers reading long reports, which they need.

## 3. codegraph_explore

- 36 calls in the window (28 returned a real answer; 8 returned the "not indexed" notice for a
  scratchpad worktree). Answers: median 24.9k characters, five files, of which about 1.9k is the
  blast radius and relationships and the rest source.
- **`maxFiles` exists** (MCP parameter, default 12; `codegraph explore --max-files N` in the shell,
  version 1.6.2). Agents already passed 2 to 8 in 16 of the calls. The output is capped at about
  25k characters, so the cap fills before the file limit does:

  | `maxFiles` | Answer size (measured, one query) | From transcripts |
  | --- | ---: | ---: |
  | 1 | 14,189 | — |
  | 2 | 21,561 | 16,231 (n=1) |
  | 3 to 8 | 24,936 to 25,063 | 24,659 to 24,953 |
  | 12 (default) | 25,105 | 24,903 (n=12) |

- Use of the answer: of the 574k source characters returned, 20% were in files the agent touched
  (Read, Edit, a command or a query naming the file) within its next 20 calls; the median answer had
  none of its files touched again. That is a weak measure (an answer can be used without touching
  its files), but it shows answers were mostly read as context, not as an edit target.
- 14 files were read again with Read, `sed` or `cat` right after codegraph had returned them (103k
  characters).
- A lower default would save at most about 10k characters per call (maxFiles 1) and lose the call
  path between files, which is what the tool is for. At 36 calls in 13 days that is under 0.4M
  characters in total, against 22.9M for bundled Bash reads. **No change recommended.**

## 4. Repeated reads

The same file read three or more times in one transcript (Read, or a single-file `sed -n`/`cat`):
47 cases in 36 of 952 transcripts, median 3 reads, at most 9. The second and later reads returned
514k characters, nearly all (510k) with no edit in between. `.github/workflows/ci.yml` accounts for 8
of the 47. This is 0.5% of the total; not worth a line in the brief.

## 5. Before and after the brief

Subagent transcripts only:

| | Transcripts | Output per transcript | Bash share | Bundled reads per transcript | Saved-output Reads per transcript |
| --- | ---: | ---: | ---: | ---: | ---: |
| before 2026-10-31 12:00 | 541 | 170k | 84% | 36.6k | 6.7k |
| after | 397 | 56k | 80% | 7.7k | 3.9k |

The drop is large but mixed with the change in task sizes. What is left per transcript after the
brief, above 6k characters per call: bundled reads 2.7k, saved-output Reads 3.3k, `git diff`/`git
show` 2.0k, long ranges and whole `cat` 1.5k. Together about 9.5k of 56k (17%).

## Rules

Each rule names its saving per subagent transcript after 2026-10-31 (the current behaviour).

| # | Rule | Saving per transcript | Where |
| --- | --- | ---: | --- |
| 1 | One file and one range per read call: no `cat a b c`, no several `sed -n` in one command, no loop that prints files. | ~2.7k (bundles are 14% of output) | brief, and a hook |
| 2 | When Bash output was too large and saved to a file, do not Read that file whole: grep it or Read it with offset and limit. Prefer filtering the command so it never overflows. | ~3.3k | brief, and a hook |
| 3 | `git diff`/`git show`: `--stat` first, then one path, and `\| head -150` when it is long; `git show REF:path \| sed -n 'A,Bp'` for old versions. | ~2.0k | brief |
| 4 | Ranges of at most 120 lines, no whole `cat` of source. | ~1.5k | already in the brief; a hook would enforce it |

Rules not made, because the numbers do not support them: a lower codegraph `maxFiles` default
(section 3), anything about `go test`, `gh run view`, kubectl or make output (each under 2%, already
filtered), and repeated reads (0.5%).

### Hooks and settings rather than brief lines

A brief line is advice; the reads above happened while the brief already said "read the part you
need". Two PreToolUse hooks in `.claude/settings.json`, next to `hack/hooks/agent-spawn-check.sh`,
would enforce rules 1, 2 and 4:

- **Read of `*/tool-results/*` without `limit`**: deny with "grep the file or Read it with offset and
  limit". Exact match, no false positives. Covers 5.2M characters in the window.
- **Bash that reads files**: deny a command with two or more `sed -n … <file>` or a `cat` of two or
  more files (outside a heredoc write), and a `cat` of a `.go`, `.ts`, `.tsx`, `.md` or `.yaml` file
  without a pipe. The message names the Read tool with offset and limit. This needs a test file
  like `agent-spawn-check_test.sh`, because the patterns are heuristic (`grep`, `cat … | grep` and
  heredoc writes are fine and must pass).

AGENTS.md needs no change: these are agent habits, and AGENTS.md already points subagents at the
brief.
