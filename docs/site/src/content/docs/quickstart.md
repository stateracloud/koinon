---
title: Quickstart
description: From helm install to an approved pull request in about 15 minutes.
---

You will install Koinon, sign in, add a model provider, connect a GitHub App, create the
repo-helper agent, give it a ticket and approve the pull request it proposes.

The clock starts at `helm install`. The prerequisites are not on it. Install them first
([Prerequisites](/prerequisites/)).

You need:

- a cluster with the prerequisites, and `kubectl`, `helm` and `jq` pointed at it;
- a GitHub App installed on one repository you can write to ([GitHub App](/guides/github-app/));
- an OpenRouter API key, or an Ollama server ([Models](/guides/models/));
- Koinon's images and chart. There is no published release yet, so work from a clone of the
  repository and [build the images](#build-the-images).

Run the commands from the root of the clone. The shell commands in blocks marked `e1` are the ones
the end-to-end test runs.

## 1. Write your values

Save this as `values.yaml`. It uses Kata. Change `image.registry` and `image.tag` to
where you pushed the images ([Build the images](#build-the-images)), and `openshell.address` if your
gateway lives elsewhere. The chart runs only the flavors it has an entry for, so this file turns off
Hermes and pi, whose images the build below does not make.

```yaml chart-values
image:
  registry: registry.example.com/koinon
  tag: v0.1.0

flavors:
  hermes: null
  pi: null

openshell:
  address: openshell.openshell.svc:8080
  clientTLSSecret: openshell-client-tls
  runtimeClassName: kata

```

The model provider is not a chart value. You add it in the UI after you sign in (step 4), and
[Models](/guides/models/) explains the options. The chart also creates the key-encryption keys that
protect stored provider keys; see [Back up the encryption keys](#back-up-the-encryption-keys).

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

## 4. Add a model provider

Agents call a model through agentgateway, with a key that never reaches their sandbox. You add the
provider in the UI.

1. In the sidebar, under **Settings**, open **Providers** and press **Add provider**.
2. Enter a **Name** (for example `openrouter`), choose the **Provider type** (OpenRouter or Ollama),
   check the **URL** and enter your **API key**. For Ollama, use your server's URL and leave the key
   empty. Press **Add provider**. Koinon encrypts the key and never shows it again.
3. On the new provider press **Add model**. Enter the **Model name** (for OpenRouter, for example
   `deepseek/deepseek-v4.1-flash`) and press **Add model**. The model appears as `default/<model name>`.

Koinon prices a model from its default price list and uses the price for budgets. If the list does not
have your model, open **Prices** under **Settings** and press **Add price**
([Prices](/guides/prices/)). For OpenRouter you can also turn on the
routing guard, which sends the model only to hosts that keep no data. The first model of the org is the
default for every agent. A new API key reaches the gateway through Kubernetes' Secret sync, which
can take about three minutes; a run that starts sooner can fail, so do this step early. If the URL is refused, the form says why; see
[Models](/guides/models/#add-a-provider-in-the-ui).

### Declare the provider with a Secret instead

A GitOps install can skip the UI and declare the provider with a Secret in Koinon's namespace. Save
this as `model-provider.yaml`, with your OpenRouter key (or an Ollama URL), and apply it. It can be
applied before or after the install. A provider declared this way is read-only in the UI.

```yaml model-provider
apiVersion: v1
kind: Secret
metadata:
  name: openrouter
  namespace: koinon
  labels:
    koinon.dev/secret-type: model-provider
    koinon.dev/org: default
stringData:
  name: openrouter
  provider: openrouter
  url: https://openrouter.ai/api/v1
  apiKey: sk-or-replace-me
  models: |
    - name: deepseek/deepseek-v4.1-flash
      guard: true
```

Koinon prices this model from its default price list, and uses the price for budgets
([Prices](/guides/prices/)). `guard: true` is the routing guard. The first model of the org is the
default for every agent.


```sh e1=model-provider
kubectl apply -f model-provider.yaml
```

## 5. Connect GitHub

Create a GitHub App first. [GitHub App](/guides/github-app/) lists the permissions.

1. Open the Connections page and press **Connect GitHub**.
2. Enter a **Name**, the **App ID**, the **Installation ID** and the **Private key** (the `.pem`
   file's text).
3. Press **Create and test connection**. Koinon signs in as the App, lists the repositories of the
   installation and shows them under **Installation repositories**. A failed check shows its message.

## 6. Create the repo-helper agent

1. Open **Agents** and press **New agent**.
2. Choose **Repo helper**, then **Next**. The template gives the agent read access to one
   repository, the right to propose pull requests and issue comments (each needs your approval), a
   30-minute cap on a run and a spend limit of $10.00 per day.
3. Choose the **OpenCode** flavor, then **Next**. The wizard also lists Hermes and pi; this install
   has no images for them, so pick OpenCode.
4. Pick the repository in **Repository 1**, then **Next**.
5. Press **Create draft**, then **Activate**.

The agent's model is the first model of the org's providers. It starts with the template's spend limit
as its own daily limit, which an admin can change ([Budgets](/guides/budgets/)). See
[Changing in v0.1](#changing-in-v01).

## 7. Create a ticket

1. On the agent's page, follow **Create a ticket for it on the board**.
2. Enter a **Title** and a **First message (optional)**. For example: "Add hello.md" and "Please add
   a hello.md that says hello from koinon."
3. Press **Create ticket**.

Koinon starts a sandbox for the agent. The agent clones the repository (read-only), makes the
change and calls `create_pull_request`. Its reply streams into the ticket's timeline. The first run
takes longer, because the sandbox starts from nothing.

## 8. Approve

The call parks the run. The ticket moves to **Waiting on you** and shows an approval card with the
branch, the title and every changed file. Nothing has reached GitHub yet.

1. Read the files in the card.
2. Press **Approve**.

Koinon creates the branch, the commit and the pull request through the GitHub API, exactly once. The
run resumes. The agent posts the pull request URL on the ticket and the run ends as `succeeded`.
Open the pull request on GitHub. The **Run costs** section of the ticket shows the tokens and the
spend of the run.

## Changing in v0.1

Providers, models, prices and budgets per org have UI pages, Secrets and an admin API under
`/admin/v1` (plan items PV1, PV2, BG1 and W4). ADR 0056 and ADR 0058 decided more for v0.1. It is
**not built yet**, so these pages do not describe it as available.

- The Kubernetes-mode gateway (plan item G2). See [Gateway modes](/guides/gateway-modes/).

[ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md)
and [ADR 0058](https://github.com/stateracloud/koinon/blob/main/docs/adr/0058-gateway-modes.md) have
the decisions.

## Back up the encryption keys

`koinon-api` created a Secret, `koinon-kek`, on its first start, with the key that encrypts the provider
keys and connection secrets in the database. (The chart renders no key material, so a re-render can
never replace it.) Back it up now. Without it those values cannot be read, and the backup of
the database does not help.

```sh
kubectl -n koinon get secret koinon-kek -o yaml > koinon-kek-backup.yaml
```

Keep the file where you keep your other secrets. Rotation and the compromise procedure are in the
[runbook](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md).

## Build the images

There is no published release yet (plan item REL1), so you build and push the images yourself. You
need Go, `ko`, Docker with buildx, Node and pnpm, and a registry your cluster can pull from. Run this
from the root of the clone. Replace `registry.example.com/koinon` and `v0.1.0` with your registry and
a tag, the same ones as `image.registry` and `image.tag` in `values.yaml`.

```sh
# 1. The web UI. koinon-api embeds web/dist when built with the webui tag.
pnpm -C web install --frozen-lockfile
pnpm -C web build

# 2. The three Koinon images: registry.example.com/koinon/koinon-api:v0.1.0 and so on.
KO_DOCKER_REPO=registry.example.com/koinon GOFLAGS=-tags=webui \
  ko build --base-import-paths --tags v0.1.0 ./cmd/koinon-api ./cmd/koinon-reconciler ./cmd/koinon-agentd

# 3. The OpenCode flavor image: registry.example.com/koinon/flavor-opencode:v0.1.0.
docker buildx build --push --build-context common=flavors/common \
  --tag registry.example.com/koinon/flavor-opencode:v0.1.0 flavors/opencode
```

Without `-tags=webui`, `koinon-api` serves no UI. Without `--base-import-paths`, `ko` gives the images
other names than the chart expects. The chart takes the flavor image tag from `image.tag`. The images
have to be pullable by the cluster; add `imagePullSecrets` for a private registry.

The end-to-end test does not run this build. It builds with `ko` without the `webui` tag and loads
the images straight into kind, and its browser suite goes through the Vite dev server.

## Not built yet

- Published images and chart (plan item REL1).
- A recorded run on Kata with a real model (plan item REL2).

## How these steps are tested

The end-to-end test, [`test/e2e/run.sh`](https://github.com/stateracloud/koinon/blob/main/test/e2e/run.sh),
builds a kind cluster and then runs the `e1` blocks above, by name, through
[`hack/docs-e1.sh`](https://github.com/stateracloud/koinon/blob/main/hack/docs-e1.sh). It does not
keep its own copy of the commands. Its `kubectl` and `helm` are wrappers that add the kind context.
Three things differ: `values.yaml` holds the kind values with the fake GitHub instead of github.com,
the provider is the fake model server instead of OpenRouter (the Go suite declares it with
`model-provider.yaml`; the browser suite adds it on the Providers page, as step 4 does), and the
wrappers add the kind context.

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
