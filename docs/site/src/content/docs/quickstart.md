---
title: Quickstart
description: From helm install to an approved pull request in about 15 minutes.
---

You will install Koinon, sign in, add a model provider, connect a GitHub App, create the
repo-helper agent, give it a task and approve the pull request it proposes.

> **Before v0.3 there is no upgrade promise.** An upgrade between any two releases before v0.3.0
> may need a reinstall or manual steps listed in the release notes. Backups of the database and the
> encryption keys, and restoring from them, are supported. See [Upgrading Koinon](/guides/upgrading/).

The clock starts at `helm install`. The prerequisites are not on it. Install them first
([Prerequisites](/prerequisites/)).

You need:

- a cluster with the prerequisites, and `kubectl`, `helm` and `jq` pointed at it;
- a GitHub App installed on one repository you can write to ([GitHub App](/guides/github-app/));
- an OpenRouter API key, or an Ollama server ([Models](/guides/models/));
- Koinon's images and chart, from a release or built from a clone ([Get Koinon](#get-koinon)).

The shell commands in blocks marked `e1` are the ones the end-to-end test runs. They install from a
clone of the repository, so run them from its root. If you install from a release, see
[Get Koinon](#get-koinon) for the `helm install` line to use instead.

## 1. Write your values

Save this as `values.yaml`. It uses Kata. Set `image.registry` and `image.tag` to where the images
are ([Get Koinon](#get-koinon)), and `openshell.address` to where your gateway listens (the chart requires it).

```yaml chart-values
# From a release these are the defaults (ghcr.io/stateracloud/koinon, the chart's own version), so you
# can leave the image block out. Set them if you built and pushed the images yourself.
image:
  registry: registry.example.com/koinon
  tag: v0.1.0

openshell:
  address: openshell.openshell.svc:8080
  clientTLSSecret: openshell-client-tls
  runtimeClassName: kata

```

`openshell.clientTLSSecret` is required: the OpenShell gateway controls every sandbox, so Koinon
talks to it over mTLS and the chart refuses to render without the Secret. Only if a service mesh
already provides mTLS between Koinon and the gateway, set `openshell.insecure: true` instead (not
together with the Secret); the components then log a warning and the preflight reports it.

The model provider is not a chart value. You add it in the UI after you sign in (step 4), and
[Models](/guides/models/) explains the options. `koinon-api` creates the key-encryption keys that
protect stored provider keys on its first start (the chart renders none); see [Back up the encryption keys](#back-up-the-encryption-keys).

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

The chart runs the preflight, then creates the generated passwords (the database roles and the
bootstrap admin) if they do not exist, then the database migration, then starts the API, the reconciler,
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
[`web/e2e-real/task-stream.spec.ts`](https://github.com/stateracloud/koinon/blob/main/web/e2e-real/task-stream.spec.ts)
against a real install. The spec's steps are numbered like these steps. The label names in bold are
the spec's own. A check in CI fails when a bold label here is not in the spec.

1. Open http://localhost:8080/. On the page **Sign in to Koinon**, enter the password and press
   **Sign in**.
2. On **Choose a new password**, enter the one-time password as the **Current password**. Enter a new
   one twice, in **New password** and **Repeat new password**. Press **Change password**.
   You land on the **Inbox**, with the list **Waiting on you**. The board is a
   secondary view at `/board`. You are the owner of the default org.

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
3. Choose the **OpenCode** flavor, then **Next**. OpenCode is the only flavor in v0.1.
4. Pick the repository in **Repository 1**, then **Next**.
5. Press **Create draft**, then **Activate**.

The agent's model is the first model of the org's providers. It starts with the template's spend limit
as its own daily limit, which an admin can change ([Budgets](/guides/budgets/)). See
[Changing in v0.1](#changing-in-v01).

## 7. Create a task

1. On the agent's page, follow **Hand it a task from the inbox**.
2. Enter a **Title** and a **First message (optional)**. For example: "Add hello.md" and "Please add
   a hello.md that says hello from koinon."
3. Press **Create task**. The task opens as a conversation next to the inbox. Follow **Open task**
   to see its page.

Koinon starts a sandbox for the agent. The agent clones the repository (read-only), makes the
change and calls `create_pull_request`. Its reply streams into the task's timeline. The first run
takes longer, because the sandbox starts from nothing.

## 8. Approve

The call parks the run. The task moves to **Waiting on you** and shows an approval card with the
branch, the title and every changed file. Nothing has reached GitHub yet.

1. Read the files in the card.
2. Press **Approve**.

Koinon creates the branch, the commit and the pull request through the GitHub API, exactly once. The
run resumes. The agent posts the pull request URL on the task and the run ends as `succeeded`.
Open the pull request on GitHub. The **Run costs** section of the task shows the tokens and the
spend of the run.

## Where next

- [Tasks and the inbox](/guides/tasks-and-inbox/), and how agents ask you questions
  ([Questions from agents](/guides/ask-user/));
- start work without a person: [Triggers](/guides/triggers/);
- stop approving the same action again and again: [Scoped approvals](/guides/scoped-approvals/);
- use your own code host: [GitHub Enterprise Server](/guides/ghes/) or [Gitea](/guides/gitea/);
- keep agents' homes across restarts: [Home checkpoints](/guides/home-checkpoints/).

## Changing in v0.1

Providers, models, prices and budgets per org have UI pages, Secrets and an admin API under
`/admin/v1` (plan items PV1, PV2, BG1 and W4). Both gateway modes are built
([Gateway modes](/guides/gateway-modes/)).

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

Keep the file where you keep your other secrets. The database needs a backup too, and the two must be
taken together: [Back up and restore](/guides/backup-restore/) has both. Rotation and the compromise procedure are in the
[runbook](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md).

## GitOps (Argo CD, Flux)

The chart renders the same manifests every time: it reads nothing from the cluster, so it renders no
password and no gateway config. The generated passwords come from a hook Job, `koinon-api
ensure-secrets`, that creates a Secret only if it is missing and never changes an existing one.
Argo CD runs it as a PreSync hook and Flux's helm-controller as a Helm hook. The gateway's config
ConfigMap is created by the reconciler, so a sync never resets it. Syncing an unchanged release
changes no password and no gateway config.

Argo CD renders every sync as an install, so the database migration cannot be a Helm hook there.
The chart marks it for Argo as a Sync hook in sync wave 1, recreated on every sync, and puts the
koinon Deployments in wave 2. A sync therefore runs the preflight and the password Job (PreSync),
brings up the database, runs the migration, and only then rolls out the new pods. Argo needs
nothing from you: use the chart as it is, with Argo's Helm source. A sync that changes nothing reruns
the migration, which does nothing; the Job shows in the Application as a hook.
Flux's helm-controller runs real installs and upgrades and uses the Helm hooks.

To manage the passwords yourself, set `postgres.cnpg.existingSecrets.<role>` (Secrets with the keys
`username` and `password`, type `kubernetes.io/basic-auth`), `bootstrapAdmin.existingSecret`,
`api.otlpToken.existingSecret` (a Secret with a `token` key: the bearer token of the internal OTLP
listener) and, in standalone gateway mode, `gateway.standalone.sessionKey.existingSecret`. With all of
them set the chart renders no hook Job.

The Secrets the Job created are not owned by the release, so `helm uninstall` leaves them (as it
leaves `koinon-kek`). To start over, delete them: `kubectl -n koinon delete secret -l
koinon.dev/release=koinon,koinon.dev/generated=password`.

An install made with an earlier chart had the chart render these Secrets, so `helm upgrade` would
delete them after the hook keeps them. Before that first upgrade, mark them to be kept:
`kubectl -n koinon annotate secret koinon-db-migrate koinon-db-api koinon-db-reconciler
koinon-db-agentd koinon-bootstrap-admin helm.sh/resource-policy=keep`.

### Upgrading to the OTLP receiver token

The first upgrade to a chart that has `koinon-otlp-token` makes koinon-api require a bearer token on
its internal OTLP listener. Until the gateway has the token (the reconciler renders the new gateway
config, or the controller applies the new policy), its access-log exports are refused with a 401 and
agentgateway does not retry them, so the spend of requests in that window (usually under a minute) is
missing from the ledger. Upgrade at a quiet time. Later rotations lose nothing while `previous` is kept
(see the [runbook](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md)).

## Get Koinon

### From a release

A release tag (for example `v0.1.0-rc.2`) publishes `api`, `reconciler`, `agentd`
and `flavors/opencode` to `ghcr.io/stateracloud/koinon`, and the chart to
`oci://ghcr.io/stateracloud/koinon/charts/koinon`, all at the version of the tag without the `v`. The
packages are private while the repository is private. Log in with a GitHub token that has
`read:packages`. After you create the namespace in step 2, give the cluster a pull Secret for `ghcr.io`:

```sh
echo "$GITHUB_TOKEN" | helm registry login ghcr.io -u "$GITHUB_USER" --password-stdin
kubectl -n koinon create secret docker-registry ghcr-pull --docker-server=ghcr.io \
  --docker-username="$GITHUB_USER" --docker-password="$GITHUB_TOKEN"
```

Add `global: {imagePullSecrets: [{name: ghcr-pull}]}` to `values.yaml`. It covers every pod the Koinon
chart renders, the database included. Sandbox pods are created by OpenShell, so give it the pull
Secret too: create `ghcr-pull` in the `openshell` namespace as well and set
`server.sandboxImagePullSecrets: [{name: ghcr-pull}]` on the OpenShell release
(`test/e2e/kind/openshell-values.yaml` shows the line). To pull through Artifactory, Nexus or Harbor
instead, see [Private registries and mirrors](/guides/private-registries/). Leave out the `image` block (the
registry defaults to `ghcr.io/stateracloud/koinon` and the tag to the chart's version), and install the
chart in step 2 from the registry instead of the clone:

```sh
helm install koinon oci://ghcr.io/stateracloud/koinon/charts/koinon --version 0.1.0-rc.2 \
  --namespace koinon -f values.yaml --wait --timeout 10m
```

### Build from source

Build and push the images yourself and install the chart from the clone. You
need Go, `ko`, Docker with buildx, Node and pnpm, and a registry your cluster can pull from. Run this
from the root of the clone. Replace `registry.example.com/koinon` and `v0.1.0` with your registry and
a tag, the same ones as `image.registry` and `image.tag` in `values.yaml`.

```sh
# 1. The web UI. koinon-api embeds web/dist when built with the webui tag.
pnpm -C web install --frozen-lockfile
pnpm -C web build

# 2. The three Koinon images: registry.example.com/koinon/api:v0.1.0, /reconciler and /agentd.
for name in api reconciler agentd; do
  KO_DOCKER_REPO=registry.example.com/koinon/$name GOFLAGS=-tags=webui \
    ko build --bare --tags v0.1.0 ./cmd/koinon-$name
done

# 3. The OpenCode flavor image: registry.example.com/koinon/flavors/opencode:v0.1.0.
docker buildx build --push --build-context common=flavors/common \
  --tag registry.example.com/koinon/flavors/opencode:v0.1.0 flavors/opencode
```

Without `-tags=webui`, `koinon-api` serves no UI. Without `--bare` and one repository per image, `ko` gives the images
other names than the chart expects. The chart takes the flavor image tag from `image.tag`. The images
have to be pullable by the cluster; add `global.imagePullSecrets` for a private registry (see
[Private registries and mirrors](/guides/private-registries/)).

The end-to-end test does not run this build. It builds with `ko` without the `webui` tag and loads
the images straight into kind, and its browser suite goes through the Vite dev server.

## Not built yet

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
  [`test/e2e/commands/quickstart.e1`](https://github.com/stateracloud/koinon/blob/main/test/e2e/commands/quickstart.e1),
  the commands the test runs. The file is the source: change a command there, then make the page
  show the same, in the same pull request;
- `run.sh` does not run an `e1` block, or runs one that does not exist;
- a `chart-values` block is not valid chart values;
- a bold label in the browser steps is missing from the Playwright spec;
- a version on the Prerequisites page differs from `test/e2e/kind/up.sh`, `internal/preflight` or the
  chart.
