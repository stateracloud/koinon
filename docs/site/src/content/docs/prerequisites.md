---
title: Prerequisites
description: What the cluster needs before you install Koinon, with pinned versions.
---

Koinon installs with one Helm chart. The chart does not install its prerequisites. The install
checks for them first (the preflight, see below) and fails with a clear message when one is missing.

The versions below are the ones the preflight (`internal/preflight`) and the end-to-end test use. The script
[`test/e2e/kind/up.sh`](https://github.com/stateracloud/koinon/blob/main/test/e2e/kind/up.sh)
installs all of them on a kind cluster. A check in CI keeps this page and that script in step.

| Prerequisite | Version | Needed for |
| --- | --- | --- |
| Kubernetes | 1.37 | Everything. The end-to-end test runs `kindest/node:v1.37.0`. |
| Agent Sandbox | v1.0.7 | The sandbox objects. The preflight needs a controller image tagged `v1.0.x`. |
| OpenShell gateway | 0.1.5 | Creating and running sandboxes. |
| A sandbox RuntimeClass | `kata`, or `runc` on kind | Isolation. The default is `kata` (ADR 0045). |
| CloudNativePG | 1.30.1 | The database, when `postgres.mode` is `cnpg` (the default). |
| Postgres | 18 or newer | The database, when `postgres.mode` is `external`. |
| A NetworkPolicy engine | kube-network-policies v1.1.2 on kind | Koinon's NetworkPolicies. |
| Helm | 4.3.1 | `helm install`. |

You also need `kubectl`, `jq` and a GitHub account that can create a GitHub App.

## Kubernetes and a NetworkPolicy engine

Use any cluster at version 1.37 or newer that enforces NetworkPolicy. The preflight warns, and does
not fail, when it cannot show enforcement. The chart uses standard NetworkPolicy only. It needs no
CNI-specific feature.

On a laptop, use kind v0.33.0. Its default node image is `kindest/node:v1.37.0`. kind's own network
plugin does not enforce NetworkPolicy, so install kube-network-policies:

```sh
kind create cluster --name koinon --image kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
curl -fsSL -o knp.yaml https://raw.githubusercontent.com/kubernetes-sigs/kube-network-policies/v1.1.2/install.yaml
echo "a8345bd40f74e812c7f2e13c40a841d4d6c7b1fc22e245750116072807a0f4ba  knp.yaml" | shasum -a 256 -c -
kubectl apply -f knp.yaml
```

## CloudNativePG

Skip this if you use `postgres.mode: external`.

```sh
curl -fsSL -o cnpg.yaml https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml
echo "37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb  cnpg.yaml" | shasum -a 256 -c -
kubectl apply --server-side -f cnpg.yaml
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s
```

The end-to-end test also pins the operator image to digest
`sha256:923c267ec29636db3bee20f993d0ec4973fa22998e1adad37da79e4d32b5bc07`. The chart pins its
Postgres image (18.6) by digest in `values.yaml`.

With an external Postgres, create one libpq URI per role in a Secret and name it in
`postgres.external.secretName`. The roles are `migrate`, `api`, `reconciler` and `agentd`. The
migrate role owns the schema. The other roles need only DML. The roles may share one login.

## Agent Sandbox

```sh
curl -fsSL -o agent-sandbox.yaml https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.7/sandbox-with-extensions.yaml
echo "136c10c9347cb5a6740b351444cae82f404e883c3f520272257ba785f2af0da8  agent-sandbox.yaml" | shasum -a 256 -c -
kubectl apply --server-side -f agent-sandbox.yaml
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s
```

## RuntimeClass

Koinon runs sandboxes under the RuntimeClass named in `openshell.runtimeClassName`. The default is
`kata`. Kata puts each sandbox in its own small VM. Use it anywhere you can. Install it the way your
distribution documents (on Talos, the `kata-containers` extension).

OpenShell only attaches RuntimeClasses that carry the label `openshell.ai/sandbox-attachable`. Label
yours if it does not have it.

kind has no VM runtime. For a throwaway cluster, use `runc` and say so in your values (the
[quickstart](/quickstart/) shows how):

```sh
kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runc
  labels:
    openshell.ai/sandbox-attachable: "true"
handler: runc
YAML
```

`runc` shares the node's kernel with the sandbox. Do not use it for real work.

## OpenShell gateway

Install the OpenShell chart, pinned by version and digest. The values file is the one the
end-to-end test uses,
[`test/e2e/kind/openshell-values.yaml`](https://github.com/stateracloud/koinon/blob/main/test/e2e/kind/openshell-values.yaml).
Set `server.defaultRuntimeClassName` in it to your RuntimeClass.

```sh
helm upgrade --install openshell \
  "oci://ghcr.io/nvidia/openshell/helm-chart@sha256:e88977ce447fd31c6278697af22f8437b59eae42c9fae9c0e480498bf59d3129" \
  --version 0.1.5 --namespace openshell --create-namespace \
  -f openshell-values.yaml --wait --timeout 5m
```

The chart generates a client certificate Secret, `openshell-client-tls`, in the `openshell`
namespace. The [quickstart](/quickstart/) copies it into Koinon's namespace.

OpenShell 0.1.5 pulls its supervisor image from `ghcr.io/nvidia/openshell/supervisor:0.1.5`. Koinon's
admission policy allows exactly that image. If you mirror it, add your reference to
`sandboxPolicy.supervisorImages`.

## The preflight

`helm install` runs a pre-install Job, `koinon-api preflight`. It fails the release when it finds
any of these:

- Agent Sandbox is missing, or its controller image is not `v1.0.x`.
- The OpenShell gateway is not reachable at `openshell.address`, or its version is not 0.1.5.
- The RuntimeClass in `openshell.runtimeClassName` does not exist.
- The CloudNativePG operator is missing (when `postgres.mode` is `cnpg`).
- The agentgateway `GatewayClass` is missing (when `gateway.mode` is `kubernetes`).

It warns, and does not fail, when Kata guest seccomp is off (ADR 0048), or when your cluster does
not enforce NetworkPolicy. Turn the seccomp probe off with `preflight.seccompProbe.enabled: false`
on `runc`, where it does not apply.

## Not built yet

Koinon has no published images or chart yet. They arrive with the release workflow (plan item REL1).
Until then, install from a clone of the repository (`charts/koinon`) and build the images with `ko`.
The [quickstart](/quickstart/) says where this shows.
