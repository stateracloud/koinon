<!-- SPDX-License-Identifier: Apache-2.0 -->

# Real-model test windows (RT1 to RT3)

CI never calls a paid model: it uses the fake model server. Before a release, though, Koinon is run
once against a real model, on a real cluster, through three stories, so that what the fakes cannot
show (a model's behaviour with the tools, real spend, long runs) is measured. This page is how to do
that on **your own cluster, with your own model key and your own budget**. Nothing here needs the
maintainers' infrastructure.

Run it in a short window with a hard spend limit, locally, never from CI.

## What the three stories measure

All three use the RT fixtures (`test/fixtures/rt1/README.md`): synthetic MCP servers behind
agentgateway, with planted anomalies, so the result can be checked.

| Story | What the agent does | What it shows |
| --- | --- | --- |
| RT1, daily FinOps cost check | Reads yesterday's cloud bill, CI cost and inference spend through three MCP servers and reports the planted anomalies (the VPC cost, the `checkout-web` CI cost, the `support` team's model spend) | Tool use over remote MCP through the gateway, scheduled runs, the cost of a routine run |
| RT2, long-running research task | Researches a question over many turns with web search and page reading, writing notes | Long runs: run limits, compaction, home checkpoints, the wait and resume paths |
| RT3, weekly dependency-upgrade agent | Looks over a repository (the fixtures' fictional handbook pushed to your Git host, or your own sandbox repo), proposes upgrades, asks `ask_user` before opening a pull request, and opens it on approval | Approvals and the inbox, the code-host connector, scoped approvals |

For each story record the transcript, wall-clock timings, the spend per run from the **Budgets** page
and anything that surprised you. Report problems as issues with the run's ID.

## Before you start

1. A cluster you may break: `make dev-kind-up` (runc, one node) is enough for all three stories.
   For the isolation tiers that need a VM runtime (Kata), use a cluster that has one and a remote
   Tilt profile or a release install ([loop.md](loop.md#remote-cluster)).
2. Koinon installed on it: `make dev` (Tilt) or `helm install` of the chart.
3. A model provider with a key you own and a budget you are willing to spend. Pick a model that
   handles tool calls well; a small local model works for plumbing but not for the stories.
4. Always pass an explicit `KUBECONFIG` or `--context` to every tool (see AGENTS.md). Do not rely on
   the default context.

## Steps

### 1. Fixtures

On kind:

```sh
hack/rt1-fixtures-kind.sh        # builds the five images, loads them into kind-koinon-dev, applies overlays/kind
```

On another cluster, push the images to a registry the cluster can pull from, then apply the remote
overlay with your registry in place of the placeholder:

```sh
hack/rt1-fixtures-build.sh "$REGISTRY"       # needs docker, ko, crane; prints the digests
kubectl --context "$CONTEXT" create namespace koinon-fixtures
# create rt1-fakecicost-token (key: token) and rt1-searxng-secrets (keys: secret_key, mcp_token)
# in koinon-fixtures; test/fixtures/rt1/overlays/kind/secrets.example.yaml shows the shape
kustomize build test/fixtures/rt1/overlays/remote \
  | sed "s#registry.example.test/koinon#$REGISTRY/koinon#g" \
  | kubectl --context "$CONTEXT" apply -f -
```

The Prometheus and cloud-bill fixtures date their data from when the pod started: **restart the pods
on the day of the test** (`kubectl --context "$CONTEXT" -n koinon-fixtures rollout restart deploy`)
so that "yesterday" is the day before. Do it after 03:00 UTC.

Let the gateway reach the fixtures: the chart's `egress.allowedInternalHosts` and
`networkPolicy.gatewayEgressTo` need the four fixture hosts and ports (listed in the fixtures README).
Helm lists replace, so keep the profile's own entries. On kind, create `hack/dev/local.yaml` (git-ignored)
containing `values: [hack/dev/values-rt1.example.yaml]` and restart `tilt up`: that file has the four
hosts plus the fake model server. On a remote cluster, copy it, replace the fake model entry with your
model endpoint and name your copy under `values:` in your local.yaml. Then register each as an MCP server
([MCP servers](../site/src/content/docs/guides/mcp-servers.md)). Give `rt1-fakecicost` and
`rt1-searxng-mcp` a bearer credential: the token in the Secrets above.

### 2. The model provider and a cost guard

Add the provider and its models in the UI ([Models](../site/src/content/docs/guides/models.md)) and
give every model a price ([Prices](../site/src/content/docs/guides/prices.md)); a model without a
price is not served, and the spend figures need prices.

Then set the guards **before** the first run ([Budgets](../site/src/content/docs/guides/budgets.md)):

- the org's monthly cap at the most you are willing to lose in total;
- each agent's daily limit at a fraction of it;
- the run limits (model requests and spend per run, and the wall clock) low enough that a loop
  cannot burn the day. A first run with a tight limit is how you find out what a story costs.

Prompt caching is on for every model that has a cache-read price in the catalog; there is no switch
(ADR 0068), so a model priced without one costs more than a cached one.

### 3. Echo test first

Some providers filter requests. OpenRouter, for instance, can mask personal data in prompts
(replacing a name with `[PERSON_NAME]`) when that is enabled for the key or account, which silently
changes what the model reads and can make a story fail or pass for the wrong reason. Before any
story, send one request that contains known personal-looking data and ask the model to repeat it back
exactly, for example from a throwaway agent whose instruction is "repeat the user's message verbatim":

> Repeat this exactly: Jane Doe, jane.doe@example.com, +1 555 0100.

If the reply contains placeholders, the provider rewrites input: turn that setting off for the key
(or use another provider) and run the echo test again. Results from a run with masking on are tainted
and should be discarded, not interpreted. Keep the echo test's request ID with the results.

### 4. Run the stories

Create the agents from the templates the stories need and run them from the UI or the API
([tasks and inbox](../site/src/content/docs/guides/tasks-and-inbox.md),
[triggers](../site/src/content/docs/guides/triggers.md),
[ask_user](../site/src/content/docs/guides/ask-user.md)):

- RT1: an agent with the three cost MCP servers, a schedule trigger or a direct task: "check yesterday's
  cloud, CI and inference costs and report anomalies". Check the report against the planted anomalies
  table in the fixtures README.
- RT2: an agent with the search MCP server and a research question of your own that takes many turns.
  Let it run past a compaction; interrupt it once and resume it.
- RT3: an agent bound to a repository on your Git host (a sandbox repository, not one you care about)
  with the code-host connector ([GitHub](../site/src/content/docs/guides/github-app.md),
  [Gitea](../site/src/content/docs/guides/gitea.md)). Answer its `ask_user` question from the inbox and
  approve the pull request.

Watch spend on the Budgets page while a story runs, and stop the window if it exceeds your plan.

### 5. Clean up

Delete the agents, revoke the provider key (or the key's limit), `kubectl --context "$CONTEXT" delete
namespace koinon-fixtures` and, on kind, `make dev-kind-down`. Keep the transcripts and a short
write-up: story, model, timings, spend, planted anomalies found or missed, surprises.

## Keeping a window safe

- Use a key made for the window, with the provider's own spend limit, and revoke it afterwards.
- The model key lives only in Koinon's provider record; never put it in a sandbox, a values file in
  git, or a log.
- Fixtures are synthetic. Do not point the stories at real customer data or a production repository.
