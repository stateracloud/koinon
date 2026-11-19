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
| `drain after SIGTERM` (koinon-gateway) | Calls of 1 s run while the process gets SIGTERM. The gateway must serve for `--shutdown-delay`, then refuse. `firstError` must come after the delay. |
| `restart under load` (agentgateway) | Calls of 1 s run while the container is stopped (SIGTERM) and started again. `firstError` to `lastError` is the outage window. |

A worker that gets an error closes its session and opens a new one for its next call, as a client does.
The open is part of that call's time and its error. A failed call is not in the latency numbers.

Each run has a 2 s warm-up that is not counted. The numbers are: calls, errors, error rate, calls per
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
(agentgateway), `internal/cmd/koinongateway/load_test.go` (koinon-gateway). All have the build tag `load`
and the Ginkgo label `load`.

## Cloud VM notes

- The cloud VM kernel has no IPv6. agentgateway then exits with "Address family not supported".
  The suite sets `IPV6_ENABLED=false` and changes the `[::]` addresses of the rendered config to `0.0.0.0`.
  Only the load suite does this.
- The load generator and the gateway share the VM's cores. Throughput is the sum, not the gateway alone.
  Read the `direct` rows to see the share of the generator.

## Baseline

First run: commit `8f43c4b`, 15 s per level, 2 s warm-up. Machine: cloud VM, Linux 6.18, 4 vCPU (Intel Xeon
2.10 GHz), 15.7 GiB, Go 1.27.4, Docker 29.10.0. The load generator, the fake server and `koinon-gateway`
share these 4 cores. agentgateway is the pinned image (`sha256:60216aaf...`).

| driver | scenario | conc. | calls | errors | calls/s | p50 | p95 | p99 |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| direct | tools/call | 1 | 27452 | 0 | 1830 | 0.48 ms | 0.94 ms | 1.51 ms |
| direct | tools/call | 16 | 61168 | 0 | 4078 | 3.24 ms | 8.98 ms | 12.70 ms |
| direct | tools/call | 64 | 73774 | 0 | 4918 | 11.34 ms | 30.81 ms | 42.53 ms |
| direct | tools/call | 256 | 81121 | 0 | 5408 | 40.71 ms | 113.66 ms | 180.10 ms |
| agentgateway | tools/call | 1 | 12068 | 0 | 805 | 1.14 ms | 1.90 ms | 2.71 ms |
| agentgateway | tools/call | 16 | 35205 | 0 | 2347 | 5.88 ms | 14.55 ms | 20.67 ms |
| agentgateway | tools/call | 64 | 29085 | 0 | 1939 | 26.30 ms | 82.62 ms | 120.50 ms |
| agentgateway | tools/call | 256 | 6311 | 1444 (18.6 %) | 421 | 148.25 ms | 1308.77 ms | 2930.81 ms |
| koinon-gateway | tools/call | 1 | 9041 | 0 | 603 | 1.39 ms | 3.45 ms | 6.29 ms |
| koinon-gateway | tools/call | 16 | 25463 | 0 | 1698 | 7.42 ms | 22.99 ms | 30.81 ms |
| koinon-gateway | tools/call | 64 | 24075 | 0 | 1605 | 35.57 ms | 83.86 ms | 102.14 ms |
| koinon-gateway | tools/call | 256 | 15772 | 2563 (14.0 %) | 1051 | 163.32 ms | 495.29 ms | 610.29 ms |

Long streams (64 calls, 10 s, 20 progress notifications each): direct 3840 of 3840 notifications
arrive; agentgateway 3840 of 3840 (run alone, 0 errors); `koinon-gateway` 0 of 3840. `koinon-gateway`
does not forward MCP progress notifications yet. The calls themselves finish with no error.

Disturbance runs (32 workers, calls of 1 s):

- `koinon-gateway` drain: the first error comes after `--shutdown-delay`. The drain is correct.
- agentgateway restart: the outage window is about 0.7 s (first error 4.0 s, last error 4.7 s).

How to read this:

- At 256 sessions both gateways drop calls. `koinon-gateway` returns "the MCP server did not answer"
  (14 %). agentgateway rejects at the transport (18.6 %). The direct run has no errors, so the limit is in
  the gateways, on this 4-core VM.
- Run the stream scenario alone for a clean number. In a full run, the agentgateway stream row that follows
  the 256-session level showed errors from the sessions of that level (94 %). A solo run had none.
- Not measured yet: the model path (waits for #279) and budget overshoot.
