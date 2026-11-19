<!-- SPDX-License-Identifier: Apache-2.0 -->

# Gateway load tests

- Status: Accepted (issue 280; ADR 0097 step 2)
- Area: engineering-process
- Tags: agentgateway, testing

The load suite measures the gateway under concurrent calls. It compares Koinon's own gateway
(`koinon-gateway`) with the agentgateway it replaces, with the same calls and the same fake MCP server.
It runs in the cloud only. It is not a CI job, and `make test` does not run it.

## Use this page when

You change the gateway (`internal/mcpgateway`, `internal/cmd/koinongateway`, the standalone driver) and
want to know what it costs in latency and throughput. Or you record a new baseline.

## What the suite does

| Scenario | What it measures |
|---|---|
| `direct` (no gateway) | The fake MCP server alone. A gateway's overhead is its numbers minus these. |
| `tools/call` | N sessions each call one tool in a loop (closed loop). N is 1, 16, 64 and 256. |
| `stream 20x500ms` | 64 calls at once, each held open for 10 s with 20 progress notifications. |
| `drain after SIGTERM` (koinon-gateway) | Calls of 1 s run while the process gets SIGTERM. The gateway must serve for `--shutdown-delay`, then refuse. No call that started before the delay ends may fail, so `firstError` must come after the delay. |
| `restart under load` (agentgateway) | Calls of 1 s run while the container is stopped (SIGTERM) and started again. `stall` (the longest time with no successful call) is the outage; `firstError` and `lastError` are when failed calls started. |

A worker that gets an error closes its session and opens a new one for its next call, as a client does.
The open is part of that call's time and its error. A failed call is not in the latency numbers.

The `tools/call` runs have a 2 s warm-up that is not counted (the stream, drain and restart runs have none). The numbers are: calls (successful ones), errors, error rate (errors over calls plus errors), calls per
second (successful calls over the run length), and p50, p95, p99 and max latency (nearest rank).

Not in the suite yet:

- **The model path.** It waits for the model path of `koinon-gateway` (#279). The fake model server
  (`cmd/koinon-fakemodel`) is the upstream then. Budget overshoot near a limit belongs to that path.
- **Replica restart with a second replica.** The suite stops one replica. A second replica behind a
  Service needs a cluster.

## Run it

You need a Linux machine, Docker and Go. The script refuses to run on macOS.

```sh
make load-gateway                          # the full run: 15 s per level, 4 levels
make load-gateway LOAD_ARGS=--quick        # 3 s, levels 1 and 16: checks that the suite runs
KOINON_LOAD_DURATION=30s hack/load-gateway.sh --out /tmp/load
```

The script writes `environment.txt` (the machine), `results.jsonl`, `report.md` and `run.log` to
`load-results/<time>/` (not committed). Every number depends on the machine: say which one you used.

How it is built:

- `koinon-fakemcp -load-tools` (one process, built by the test) is the MCP server. It has the extra tool
  `stream_progress`. The gateway injects its credential (`X-Api-Key`) on every call.
- `koinon-gateway` runs in the test process (`run`, as `main` calls it) and follows a State stream from
  that process. Its calls go over the loopback.
- agentgateway runs as a container of the pinned image. The test calls it at its bridge address, and it
  calls the fake server at the bridge's host address. No port forward is in the path.
- Both go through the same client (`internal/gateway/loadtest`).

Files, by driver: `internal/gateway/loadtest` (harness, direct run), `internal/gateway/standalone/load_test.go`
(agentgateway), `internal/cmd/koinongateway/load_test.go` (koinon-gateway). The driver tests have the build tag `load`
and the Ginkgo label `load`. The unit tests of the harness (`loadtest_test.go`) have neither and run in `make test`: they are fast and need no Docker.

## Cloud VM notes

- The cloud VM kernel has no IPv6. agentgateway then exits with "Address family not supported".
  The suite sets `IPV6_ENABLED=false` and changes the `[::]` addresses of the rendered config to `0.0.0.0`.
  Only the load suite does this.
- The load generator and the gateway share the VM's cores. Throughput is the sum, not the gateway alone.
  Read the `direct` rows to see the share of the generator.

## Baseline

First run: commit `64e9f063`, 15 s per level, 2 s warm-up. Machine: cloud VM, Linux 6.18, 4 vCPU (Intel Xeon
2.10 GHz), 15.7 GiB, Go 1.27.4, Docker 29.10.0. The load generator, the fake server and `koinon-gateway`
share these 4 cores. agentgateway is the pinned image (`sha256:60216aaf...`). The client keeps a pool of
idle connections for all sessions. (With the default pool of 2 idle connections per host, the client
opens a new connection for nearly every call, and agentgateway shows transport errors from port
exhaustion.)

| driver | scenario | conc. | calls | errors | calls/s | p50 | p95 | p99 |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| direct | tools/call | 1 | 29654 | 0 | 1977 | 0.44 ms | 0.87 ms | 1.45 ms |
| direct | tools/call | 16 | 73175 | 0 | 4878 | 2.69 ms | 7.57 ms | 10.51 ms |
| direct | tools/call | 64 | 87682 | 0 | 5845 | 9.43 ms | 25.06 ms | 34.78 ms |
| direct | tools/call | 256 | 99925 | 0 | 6662 | 34.86 ms | 88.27 ms | 129.92 ms |
| agentgateway | tools/call | 1 | 13729 | 0 | 915 | 0.99 ms | 1.64 ms | 2.31 ms |
| agentgateway | tools/call | 16 | 49532 | 0 | 3302 | 4.12 ms | 10.36 ms | 14.61 ms |
| agentgateway | tools/call | 64 | 61934 | 0 | 4129 | 14.17 ms | 32.88 ms | 44.90 ms |
| agentgateway | tools/call | 256 | 61400 | 0 | 4093 | 55.05 ms | 140.80 ms | 192.59 ms |
| koinon-gateway | tools/call | 1 | 11674 | 0 | 778 | 1.05 ms | 2.62 ms | 5.69 ms |
| koinon-gateway | tools/call | 16 | 29590 | 0 | 1973 | 6.11 ms | 20.42 ms | 27.57 ms |
| koinon-gateway | tools/call | 64 | 27400 | 0 | 1827 | 29.72 ms | 76.35 ms | 89.47 ms |
| koinon-gateway | tools/call | 256 | 17165 | 3556 (17.2 %) | 1144 | 140.62 ms | 416.00 ms | 542.84 ms |

Latency is for successful calls. Throughput is successful calls per second.

Long streams (64 calls, 10 s, 20 progress notifications each): direct 3840 of 3840 notifications
arrive; agentgateway 3840 of 3840; `koinon-gateway` 0 of 3840. `koinon-gateway` does not forward MCP
progress notifications yet. The calls themselves finish with no error.

Disturbance runs (32 workers, calls of 1 s):

- `koinon-gateway` drain: the first failed call starts at 6.0 s, which is the SIGTERM (4 s) plus `--shutdown-delay` (2 s). No call that started earlier failed. The drain is correct.
- agentgateway restart: callers wait for the new process. Failed calls started at 4.0 s to 4.1 s (the stop), and no call succeeded for 2.1 s (the outage, measured by `stall`, in a solo run of this scenario).

How to read this:

- At 256 sessions `koinon-gateway` drops calls ("the MCP server did not answer"). agentgateway and the
  direct run have none. The limit is in `koinon-gateway`, on this 4-core VM.
- At 1 to 64 sessions neither gateway has errors. agentgateway is faster than `koinon-gateway` at every
  level. At 256 sessions agentgateway has 3.6 times the throughput of `koinon-gateway`, which also drops
  calls.
- A stream run is three rounds of 64 calls of 10 s. calls/s divides by the real length of the run.
- Not measured yet: the model path (waits for #279) and budget overshoot.
