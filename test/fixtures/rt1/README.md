# RT1 fixtures

Test fixtures for the v0.1 home-lab story test (RT1, `docs/plans/v0.1.md`). The stories need tools
that agents reach only as remote MCP servers behind agentgateway. These fixtures stand in for three
cost sources (the daily FinOps cost check) and for a company docs repository (the internal-research
story). Everything is synthetic or openly licensed, runs in namespace `koinon-fixtures`, and needs
no egress at run time.

| Fixture | Directory | What agents see | MCP endpoint (in cluster) |
|---|---|---|---|
| Cloud bill, Athena-like SQL | `motherduck/` | DuckDB over FOCUS 1.0 sample billing data, read-only SQL | `http://rt1-motherduck.koinon-fixtures.svc.cluster.local:8000/mcp` |
| CI cost | `cmd/koinon-fakecicost` | REST `GET /v1/costs` and the tool `ci_costs`, bearer token required | `http://rt1-fakecicost.koinon-fixtures.svc.cluster.local:8080/mcp` |
| Inference spend | `prometheus/`, `cmd/koinon-rt1-promgen` | Prometheus with 30 days of backfilled LLM metrics, behind prometheus-mcp-server | `http://rt1-prometheus.koinon-fixtures.svc.cluster.local:8080/mcp` |
| Web research | `manifests/searxng.yaml` | SearXNG (search) and `web_url_read` (page to Markdown) via mcp-searxng, bearer token required | `http://rt1-searxng-mcp.koinon-fixtures.svc.cluster.local:8080/mcp` |
| Internal docs | `docs-repo/` | A fictional platform handbook to push to Gitea | n/a (Gitea MCP) |
| Manifests | `manifests/` | Deployments, Services, NetworkPolicies, kustomization | n/a |

## Planted anomalies

The write-up checks whether the agent found these. "Yesterday" is the UTC day before the test runs.

| Source | Anomaly | How it looks |
|---|---|---|
| Cloud bill | The cost of **Amazon Virtual Private Cloud** yesterday is exactly 3x its average of the previous 7 days (about 27 USD against 9 USD) | `SELECT CAST(ChargePeriodStart AS DATE) d, sum(BilledCost) FROM focus WHERE ServiceName = 'Amazon Virtual Private Cloud' GROUP BY 1 ORDER BY 1 DESC` |
| CI cost | The **checkout-web** repository's CI cost about doubles yesterday, because a new matrix job fans out its `test` workflow (runs and minutes of `test` go up about 3x) | `ci_costs` with `group_by=repo`, or `group_by=workflow` for the cause |
| Inference spend | The **support** team's spend on **claude-sonnet-4** doubles yesterday (cost and tokens) | `sum by (team, model) (increase(llm_cost_usd_total[1d]))` |

All other series stay within about 10 to 20 percent of their normal day-to-day variation.

## Cloud bill (`motherduck/`)

`mcp-server-motherduck` 1.1.0 (MIT) in HTTP mode, read-only, over DuckDB 1.5.6. The image downloads
`focus_sample_10000.csv` from the FinOps Foundation's FOCUS-Sample-Data repository at build time
(pinned by commit, checked by sha256) and bakes it into a DuckDB file (about 2 MB), so nothing
large is committed and the pod needs no egress. At container start `startup.py` copies the file to
`/work`, shifts every date so the data ends yesterday (30 days: one table, `focus`, 10,000 rows, AWS,
Microsoft and Oracle charges), multiplies costs by 100 so the figures look like a small company's bill,
plants the anomaly, and serves the copy read-only. Python packages are pinned in `constraints.txt`
and the base image by digest. The tool to use is `execute_query`.

Run locally:

```sh
docker build -t rt1-motherduck motherduck/
docker run --rm -p 8000:8000 --read-only --tmpfs /work:uid=10001,gid=10001 rt1-motherduck
# MCP at http://localhost:8000/mcp
```

## CI cost (`cmd/koinon-fakecicost`)

A Go fake modelled on `cmd/koinon-fakemcp`. Data is deterministic per date (seeded by the date), covers
the last 60 days up to and including today (UTC), four repositories, four workflows and four runner
types. `GET /v1/costs?from=YYYY-MM-DD&to=YYYY-MM-DD[&group_by=repo|workflow|runner]` and the MCP tool
`ci_costs` take the same parameters and return daily rows (per group with `group_by`) and a total.
Both need `Authorization: Bearer <token>` (flag `-token` or `KOINON_FAKECICOST_TOKEN`; the server
refuses to start without one) and answer 401 otherwise. `GET /_fake/requests` lists what arrived,
including unauthorised attempts and tool arguments, so the test can show that agentgateway injected the
header. `/healthz` needs no token. In the cluster the token comes from Secret `rt1-fakecicost-token`
(a placeholder; set the real value out of band and register the same value as the MCP server's
credential in Koinon).

Run locally:

```sh
export KOINON_FAKECICOST_TOKEN=koinon_test_not-a-real-token
go run ./cmd/koinon-fakecicost
curl -H "Authorization: Bearer $KOINON_FAKECICOST_TOKEN" \
  'localhost:8080/v1/costs?from=2026-10-20&to=2026-10-27&group_by=repo'
```

## Inference spend (`prometheus/`, `cmd/koinon-rt1-promgen`)

`koinon-rt1-promgen` writes OpenMetrics text (counters with `_total`, whole-second timestamps, closing
`# EOF`), deterministic for a seed and an end time: `llm_tokens_total{model,provider,team}`,
`llm_cost_usd_total{model,provider,team}` and `gpu_hours_total{gpu,team}`, every 15 minutes for 30 days,
8 LLM series per metric. The pod's init containers run promgen, then `promtool tsdb create-blocks-from
openmetrics` into an emptyDir; Prometheus 3.7.3 then serves it with 90 days of retention, no scrape
targets, listening on 127.0.0.1 only. `prometheus-mcp-server` 1.6.2 (MIT, `Dockerfile.mcp`) talks to it
over localhost and serves Streamable HTTP on :8080. It has no authentication, so the NetworkPolicy lets
only the gateway in.

Caveats:

- The data ends 3 hours before the pod starts (promtool cannot backfill the head block's range) and
  nothing adds to it. **Restart the pod on the day of a test** (`kubectl -n koinon-fixtures rollout
  restart deploy/rt1-prometheus`) so "yesterday" is the previous day, and do it after 03:00 UTC, or the
  last hours of yesterday are missing. Building the blocks takes 3 to 4 minutes.
- The cost of `claude-sonnet-4` is a blended USD price per million tokens, not a real price list.

Run locally (needs Docker):

```sh
go run ./cmd/koinon-rt1-promgen -o /tmp/rt1.om                 # -seed, -days, -end, -lag, -step
docker run --rm -v /tmp:/om:ro -v "$PWD/data:/data" --user 65534 --entrypoint /bin/promtool \
  prom/prometheus:v3.7.3 tsdb create-blocks-from openmetrics /om/rt1.om /data
docker run --rm -p 9090:9090 --user 65534 -v "$PWD/data:/data" prom/prometheus:v3.7.3 \
  --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/data --storage.tsdb.retention.time=90d
```

## Web research (`manifests/searxng.yaml`)

An unmodified SearXNG (image `searxng/searxng`, pinned by digest `sha256:0f48ffe2...`, version
2026.10.27) behind `ihor-sokoliuk/mcp-searxng` 2.9.0 (MIT, image by digest `sha256:8f1dd92a...`) in
Streamable HTTP mode with a static bearer token (`MCP_HTTP_HARDEN`, `MCP_HTTP_AUTH_TOKEN` from Secret
`rt1-searxng-secrets`, stateless). Tools: `searxng_web_search` and `web_url_read`. SearXNG's config is
`manifests/searxng-settings.yml`: JSON and HTML formats, engines duckduckgo, bing, wikipedia, github and
stackoverflow, limiter and image proxy off, no Valkey or Redis. Its secret key comes from the Secret
(placeholder; set the real one out of band). Nothing is built: the build script copies both images to Zot
by digest.

- **Internet egress.** Unlike the other fixtures, this namespace's two web-research pods may reach the
  public internet on ports 80 and 443, because searching and reading pages is their job. The
  NetworkPolicy allows `0.0.0.0/0` except 10/8, 172.16/12, 192.168/16, 169.254/16, 100.64/10 and
  127/8 (IPv4 only), so a URL cannot send them into the cluster, the LAN or a metadata service. SearXNG
  accepts connections only from mcp-searxng, and mcp-searxng only from the gateway. DNS is allowed for
  everything.
- **Licence.** SearXNG is AGPL-3.0. We run the upstream image unmodified as a separate service for
  testing; its code is not vendored, patched or bundled into any Koinon image, and `searxng-settings.yml`
  is configuration only. Keep it that way. Do not ship it as a Koinon component.
- Duckduckgo and others may answer a CAPTCHA to a home IP; wikipedia, github and bing normally work.

Run locally (public internet needed):

```sh
docker network create rt1
docker run -d --name sx --network rt1 --user 977 -e SEARXNG_SECRET=koinon_test_not-a-real-secret \
  -v "$PWD/test/fixtures/rt1/manifests/searxng-settings.yml:/etc/searxng/settings.yml:ro" \
  searxng/searxng@sha256:0f48ffe2152f388f5eaede27f96b610449cebfc890d0ad457304475a54ad7b6e
docker run -d --name mx --network rt1 -p 8081:8080 -e SEARXNG_URL=http://sx:8080 -e MCP_HTTP_PORT=8080 \
  -e MCP_HTTP_HOST=0.0.0.0 -e MCP_HTTP_HARDEN=true -e MCP_HTTP_STATELESS=true \
  -e MCP_HTTP_AUTH_TOKEN=koinon_test_not-a-real-token -e MCP_HTTP_ALLOWED_HOSTS=localhost:8081 \
  -e MCP_HTTP_ALLOWED_ORIGINS=http://localhost:8081 \
  isokoliuk/mcp-searxng@sha256:8f1dd92a9e2875cb4e8df7036785ec4c088b7600cefb611de472a352a3dcba60
export MCP_TOKEN=koinon_test_not-a-real-token
curl -s localhost:8081/mcp -H "Authorization: Bearer $MCP_TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_url_read","arguments":{"url":"https://example.com"}}}'
```

## Internal docs (`docs-repo/`)

An index and six pages of a fictional "Northwind platform handbook" (secret handling, access windows,
backup and restore, dependency upgrades, incident policy, glossary). Push the directory's contents to
a Gitea repository next to the real home-lab repository (for example `northwind/platform-handbook`), so
the agent can compare the written policy with practice. The pages are not licensed from anyone and not
real policy.

## Manifests (`manifests/`)

`kubectl apply -k test/fixtures/rt1/manifests` creates namespace `koinon-fixtures`, the three
Deployments and Services, and these NetworkPolicies: default deny ingress; ingress to the MCP ports
only from Koinon's gateway pods in namespace `koinon` (both gateway modes' labels); egress DNS only.
Every container has requests and limits, runs non-root with a read-only root file system, and no pod
mounts a service account token. Image references are `zot.apps.lab.example.invalid/koinon/rt1-<name>:<tag>`
placeholders; fill in digests at deploy (see `kustomization.yaml`).

| Pod | Containers (memory limit) | Pod limit |
|---|---|---|
| rt1-motherduck | motherduck 192Mi (about 75 MiB measured) | 192Mi |
| rt1-fakecicost | fakecicost 64Mi | 64Mi |
| rt1-searxng | searxng 256Mi (about 87 MiB measured) | 256Mi |
| rt1-searxng-mcp | mcp-searxng 128Mi (about 43 MiB) | 128Mi |
| rt1-prometheus | prometheus 144Mi (about 95 MiB), prometheus-mcp 112Mi (about 75 MiB); init: promgen 64Mi, promtool 256Mi | 256Mi at run time |

To let agents use them, the gateway must be allowed to reach the four hosts
(`rt1-motherduck:8000`, `rt1-fakecicost:8080`, `rt1-prometheus:8080` and `rt1-searxng-mcp:8080`, each
`<name>.koinon-fixtures.svc.cluster.local`): add them to the chart's
`egress.allowedInternalHosts` and `networkPolicy.gatewayEgressTo` (ports 8000 and 8080 in
`koinon-fixtures`), then register each as an MCP server (`docs/site/src/content/docs/guides/mcp-servers.md`).
For fakecicost and rt1-searxng-mcp, give the server a bearer credential so agentgateway injects the header.

## Building images

`hack/rt1-fixtures-build.sh REGISTRY [TAG]` builds four images for linux/amd64 (two with ko, two with buildx) and mirrors three (Prometheus and the two
web-research images) by digest, then pushes them
(`DRY_RUN=1` prints the commands only). By hand:

```sh
# Go images with ko (cmd/koinon-fakecicost, cmd/koinon-rt1-promgen)
KO_DOCKER_REPO=<registry>/koinon/rt1-fakecicost ko build --bare --platform linux/amd64 --tags dev ./cmd/koinon-fakecicost
KO_DOCKER_REPO=<registry>/koinon/rt1-promgen    ko build --bare --platform linux/amd64 --tags dev ./cmd/koinon-rt1-promgen
# Python images with docker buildx
docker buildx build --platform linux/amd64 --push -t <registry>/koinon/rt1-motherduck:dev test/fixtures/rt1/motherduck
docker buildx build --platform linux/amd64 --push -f test/fixtures/rt1/prometheus/Dockerfile.mcp \
  -t <registry>/koinon/rt1-prometheus-mcp:dev test/fixtures/rt1/prometheus
# Prometheus itself (promtool is in the image): a pinned copy
crane copy prom/prometheus:v3.7.3@sha256:49214755b6153f90a597adcbff0252cc61069f8ab69ce8411285cd4a560e8038 \
  <registry>/koinon/rt1-prometheus:v3.7.3
# Web research: mirrors, by digest
crane copy searxng/searxng@sha256:0f48ffe2152f388f5eaede27f96b610449cebfc890d0ad457304475a54ad7b6e \
  <registry>/koinon/rt1-searxng:2026.10.27
crane copy isokoliuk/mcp-searxng@sha256:8f1dd92a9e2875cb4e8df7036785ec4c088b7600cefb611de472a352a3dcba60 \
  <registry>/koinon/rt1-searxng-mcp:2.9.0
```

## Attribution and licences

- The cloud-bill data derives from the FinOps Foundation's FOCUS 1.0 Sample Data
  (https://github.com/FinOps-Open-Cost-and-Usage-Spec/FOCUS-Sample-Data), (c) FinOps Foundation,
  licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). Changes: costs scaled by
  100, dates shifted, one anomaly planted, text columns cast to numbers. See `motherduck/NOTICE`.
  The data is not committed here; it is downloaded at image build time.
- `mcp-server-motherduck` (MIT), `prometheus-mcp-server` (MIT), `mcp-searxng` (MIT), SearXNG (AGPL-3.0, see above), DuckDB (MIT) and Prometheus
  (Apache-2.0) are used unmodified from their releases.
- Everything under `cmd/` and the rest of this directory is Apache-2.0 like Koinon.
