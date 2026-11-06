# Replay corpora

Corpora of the replay suite (`replay_test.go`, #352, ADR 0091). The format is documented at the top
of `corpus_test.go`: one JSON object per line, the org's declarations first (`org`, `server`, `tool`),
then what happened in order (`feed` for agentd's report of a sandbox read, `call` for a tools/call with
its arguments and result).

**These are hand-built, not recordings.** `gen_corpus.py` writes both files from templates and a
fixed seed. The benign one is shaped like the rc.9 traffic of deep dive G1 and keeps the counts of the
decision comment on #265 (218 fetches to outside hosts, 272 writes); nothing in it came from a real
run, so the numbers the suite prints from it are a harness self-test, not a measurement. Rerunning
`python3 -I gen_corpus.py` must leave both files unchanged; after editing the generator, regenerate
and commit the output. `generator_test.go` runs the generator into a scratch tree and fails when the
committed files differ (it skips without `python3`).

| File | What it holds |
|---|---|
| `handbuilt-benign.jsonl` | 17 runs in 9 generations (two runs each, and one long research run). Writes into the org's own private repo `northwind/platform-handbook` (internal, the #352 false alarm), tickets (internal), a trusted docs host (internal), a cost query (none), Slack and mail (external-high), fetches and searches (external-low). Private reads come from the RT1 handbook pages, shell output and the cost server; the untrusted leg from fetch and search results. The long run goes over the byte cap. |
| `handbuilt-leaks.jsonl` | Planted leaks: five private sources (two RT1 canaries, a handbook rule, a fake token in shell output, an MCP result) in 28 shapes each, plus each RT1 canary line base64-encoded, each replayed with the private leg only and with both legs. `expect` says what the ADR promises: `caught_before_both_legs` (the text match, or the URL rule for a fetch), `caught` (with both legs), `pass` (a write into the org's private repo is no leak), `not_promised` (arguments out of call order). |

Everything is fake: the tokens are `koinon_test_not-a-real-token...`, the canaries
`koinon-canary-rt1-...-not-a-secret`, the hosts `example` domains or public documentation URLs that are
never fetched (the suite has no network), the connection ID `00000000-0000-4000-8000-000000000352`.

`model_replay_test.go` replays a sample of both corpora (each server/tool and expectation, the split
shapes, the run over the cap) with the in-process fake model in the loop: the model emits each call
as a tool_use and the guard decides the arguments the model sent. One case differs from the direct
replay because the fake model sorts argument keys; the test names it (`keyOrder`) and why.

A recorded corpus does not belong here: it is measured from its own path with
`KOINON_LEAKGUARD_CORPUS` (`docs/dev/real-model-tests.md`).
