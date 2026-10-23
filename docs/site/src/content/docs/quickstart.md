---
title: Quickstart
description: From helm install to an approved pull request in about 15 minutes.
---

You will install Koinon, sign in, connect a GitHub App, create the repo-helper agent, give it a
ticket and approve the pull request it proposes.

The clock starts at `helm install`. The prerequisites are not on it. Install them first
([Prerequisites](/prerequisites/)).

You need:

- a cluster with the prerequisites, and `kubectl`, `helm` and `jq` pointed at it;
- a GitHub App installed on one repository you can write to ([GitHub App](/guides/github-app/));
- an OpenRouter API key, or an Ollama server ([Models](/guides/models/));
- Koinon's images and chart. There is no published release yet, so work from a clone of the
  repository and build the images with `ko`. See [Not built yet](#not-built-yet).

Run the commands from the root of the clone. The shell commands in blocks marked `e1` are the ones
the end-to-end test runs.

## 1. Write your values

Save this as `values.yaml`. It uses OpenRouter and Kata. Change `image.registry` to the registry
that holds your images, and `openshell.address` if your gateway lives elsewhere.

```yaml chart-values
image:
  registry: registry.example.com/koinon

openshell:
  address: openshell.openshell.svc:8080
  clientTLSSecret: openshell-client-tls
  runtimeClassName: kata

model:
  baseUrl: https://openrouter.ai/api/v1
  provider: openrouter
  apiKeySecret:
    name: koinon-openrouter
    key: api_key
  models:
    - name: deepseek/deepseek-v4.1-flash
      rates: {input: "0.30", output: "1.20"}
      overrides:
        provider:
          zdr: true
          data_collection: deny
          only: [fireworks, together, deepinfra, amazon-bedrock, google-vertex]
          allow_fallbacks: false
```

The first model in `model.models` is the default for every agent. `rates` are US dollars per million
tokens. Koinon needs a rate to serve a model, and uses it for budgets. The `overrides` block is the
routing guard. [Models](/guides/models/) explains it.

On kind, use `runc`. Add these lines and leave the rest:

```yaml chart-values
openshell:
  runtimeClassName: runc

sandboxPolicy:
  runtimeClasses: [runc]

preflight:
  seccompProbe:
    enabled: false
```

## 2. Install

Create the namespace and copy OpenShell's client certificate into it. Koinon uses the certificate to
talk to the gateway over mTLS.

```sh e1=namespace
kubectl create namespace koinon --dry-run=client -o yaml | kubectl apply -f -
```

```sh e1=client-cert
kubectl -n openshell get secret openshell-client-tls -o json | jq --arg ns koinon '{apiVersion, kind, type, data, metadata: {name: .metadata.name, namespace: $ns}}' | kubectl apply -f -
```

Store the model key. Only agentgateway mounts this Secret. Sandboxes never see it.

```sh e1=model-key
kubectl -n koinon create secret generic koinon-openrouter --from-literal=api_key="$OPENROUTER_API_KEY" --dry-run=client -o yaml | kubectl apply -f -
```

Install the chart. This is the start of the 15 minutes.

```sh e1=install
helm install koinon charts/koinon --namespace koinon -f values.yaml --wait --timeout 10m
```

The chart runs the preflight, then the database migration, then starts the API, the reconciler,
agentd and the bundled agentgateway. If the preflight fails, its message names what is missing and
how to fix it. The install notes print the next two commands.

## 3. Sign in

Read the one-time password of the bootstrap admin (user `admin`):

```sh e1=password
kubectl -n koinon get secret koinon-bootstrap-admin -o jsonpath='{.data.password}' | base64 -d
```

Open the UI on http://localhost:8080. Leave this command running in its own terminal:

```sh e1=open
kubectl -n koinon port-forward svc/koinon-api 8080:8080
```

The browser steps below are run by the Playwright spec
[`web/e2e-real/ticket-stream.spec.ts`](https://github.com/stateracloud/koinon/blob/main/web/e2e-real/ticket-stream.spec.ts)
against a real install. The spec's steps are numbered like these steps. The label names in bold are
the spec's own. A check in CI fails when a bold label here is not in the spec.

1. Open http://localhost:8080/. On the page **Sign in to Koinon**, enter the password and press
   **Sign in**.
2. On **Choose a new password**, enter the one-time password as the **Current password**. Enter a new
   one twice, in **New password** and **Repeat new password**. Press **Change password**.
   You land on the board, with the columns **To do**, **In progress**, **Waiting on you** and
   **Done**. You are the owner of the default org.

## 4. Connect GitHub

Create a GitHub App first. [GitHub App](/guides/github-app/) lists the permissions.

1. Open the Connections page and press **Connect GitHub**.
2. Enter a **Name**, the **App ID**, the **Installation ID** and the **Private key** (the `.pem`
   file's text).
3. Press **Create and test connection**. Koinon signs in as the App, lists the repositories of the
   installation and shows them under **Installation repositories**. A failed check shows its message.

## 5. Create the repo-helper agent

1. Open **Agents** and press **New agent**.
2. Choose **Repo helper**, then **Next**. The template gives the agent read access to one
   repository, the right to propose pull requests and issue comments (each needs your approval), a
   30-minute cap on a run and a spend limit of $10.00 per day.
3. Choose the **OpenCode** flavor, then **Next**. Hermes and pi are also available.
4. Pick the repository in **Repository 1**, then **Next**.
5. Press **Create draft**, then **Activate**.

The agent's model is the first one in `model.models`. Its spend limit is the template's `spendUSD`.
See [Changing in v0.1](#changing-in-v01).

## 6. Create a ticket

1. On the agent's page, follow **Create a ticket for it on the board**.
2. Enter a **Title** and a **First message (optional)**. For example: "Add hello.md" and "Please add
   a hello.md that says hello from koinon."
3. Press **Create ticket**.

Koinon starts a sandbox for the agent. The agent clones the repository (read-only), makes the
change and calls `create_pull_request`. Its reply streams into the ticket's timeline. The first run
takes longer, because the sandbox starts from nothing.

## 7. Approve

The call parks the run. The ticket moves to **Waiting on you** and shows an approval card with the
branch, the title and every changed file. Nothing has reached GitHub yet.

1. Read the files in the card.
2. Press **Approve**.

Koinon creates the branch, the commit and the pull request through the GitHub API, exactly once. The
run resumes. The agent posts the pull request URL on the ticket and the run ends as `succeeded`.
Open the pull request on GitHub. The **Run costs** section of the ticket shows the tokens and the
spend of the run.

## Changing in v0.1

Today a single model upstream and the template's budget are set in Helm values. ADR 0056 and ADR 0058
decided larger changes for v0.1. They are **not built yet**, so these pages do not describe them as
available.

- Providers, models and prices per org, in the UI and in an admin API under `/admin/v1`. Plan items
  PV1 and PV2. Until then, use `model.*` in Helm values as above.
- Org and per-agent budgets that admins set. The template's `spendUSD` becomes only the default.
  Plan item BG1. Until then, the template's `spendUSD` is the agent's limit per UTC calendar day.
- The Providers, models and budgets pages in the UI (plan item W4).
- The Kubernetes-mode gateway (plan item G2). See [Gateway modes](/guides/gateway-modes/).

[ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md)
and [ADR 0058](https://github.com/stateracloud/koinon/blob/main/docs/adr/0058-gateway-modes.md) have
the decisions.

## Not built yet

- Published images and chart (plan item REL1). Build the Koinon images with `ko build
  ./cmd/koinon-api ./cmd/koinon-reconciler ./cmd/koinon-agentd`, and a flavor image with
  `make flavor-opencode`. Push them to your registry and set `image.registry` and `image.tag`.
- A recorded run on Kata with a real model (plan item REL2).

## How these steps are tested

The end-to-end test, [`test/e2e/run.sh`](https://github.com/stateracloud/koinon/blob/main/test/e2e/run.sh),
builds a kind cluster and then runs the `e1` blocks above, by name, through
[`hack/docs-e1.sh`](https://github.com/stateracloud/koinon/blob/main/hack/docs-e1.sh). It does not
keep its own copy of the commands. Its `kubectl` and `helm` are wrappers that add the kind context.
Two things differ: `values.yaml` holds the kind values with the fake model server and the fake GitHub
instead of OpenRouter and github.com, and `OPENROUTER_API_KEY` is a dummy.

`make docs-check` runs on every pull request. It fails when:

- the `e1` blocks differ from
  [`test/e2e/quickstart.golden.sh`](https://github.com/stateracloud/koinon/blob/main/test/e2e/quickstart.golden.sh),
  the commands the test runs. Change a command and the check shows the difference. Update the golden
  file in the same pull request;
- `run.sh` does not run an `e1` block, or runs one that does not exist;
- a `chart-values` block is not valid chart values;
- a bold label in the browser steps is missing from the Playwright spec;
- a version on the Prerequisites page differs from `test/e2e/kind/up.sh`, `internal/preflight` or the
  chart.
