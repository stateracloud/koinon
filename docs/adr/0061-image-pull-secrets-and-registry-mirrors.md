# 0061. Image pull secrets and registry mirrors: Koinon rewrites its images; OpenShell pulls for sandboxes

- Status: Accepted
- Date: 2026-10-26
- Closes: old issue 130
- Relates to: [0002](0002-namespace-per-org-isolation.md), [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md)

## Context

An operator needs to pull every image through credentials, and often through an internal pull-through
registry (Artifactory, Nexus, Harbor). Koinon's chart ran pods without pull secrets in two places
(standalone agentgateway and the CNPG Cluster), had no way to rewrite a registry, and sandboxes got no
pull secret at all: the sandbox API (`SandboxTemplate`) has no pull-secret or ServiceAccount field.
The workload pod and the supervisor pod are created by OpenShell and the Agent Sandbox controller.

The first decision on the issue was that the reconciler would copy a pull Secret into each org's
workspace namespace and attach it to the sandbox ServiceAccount. Research against OpenShell 0.1.5 and
Agent Sandbox v1.0.7 found that OpenShell already does the equivalent: with `server.sandboxImagePullSecrets`
the gateway copies the named Secrets into the workspace namespace for every sandbox generation, as
immutable copies owned by the Sandbox, and puts them in `imagePullSecrets` of both pods.

## Decision

1. **Sandbox pods get their pull secret from OpenShell's `server.sandboxImagePullSecrets`.** Koinon's chart
   does not copy Secrets and does not patch ServiceAccounts. The docs tell the operator to set it, and the
   preflight reads OpenShell's gateway ConfigMap and warns when pull secrets or a mirror are configured and
   the gateway has none.
2. **The chart rewrites the registry of every image it runs or names**, with one helper
   (`koinon.imageRef`): `global.registryMirrors` (per upstream host) wins over `global.imageRegistry` (one
   host for everything); path, tag and digest are kept. It covers Koinon's three images, the hook Jobs,
   the preflight probe pods, flavor images, standalone agentgateway and the CNPG image.
3. **`global.imagePullSecrets`** (names or `{name}` maps; the old top-level `imagePullSecrets` is a
   deprecated alias, merged and de-duplicated) goes on every pod the chart renders, including standalone
   agentgateway and the CNPG Cluster (`spec.imagePullSecrets`), and into the preflight probe pods.
4. **The default `sandboxPolicy.supervisorImages` is derived**: empty means OpenShell 0.1.7's supervisor
   image run through the same rewrite, so a mirror cannot make the admission policy deny every supervisor
   pod. A list set explicitly is used as written. The preflight compares the list with the gateway's
   `supervisor_image` and warns on a mismatch.
5. Pre-installed platform components (OpenShell, the CNPG operator, Agent Sandbox, the agentgateway
   controller) are out of scope. The guide says which values of their charts to set. Node-level
   containerd `hosts.toml` mirrors plus kubelet credentials remain the alternative that touches no
   image reference.

## Rejected: Koinon copies the pull Secret into workspace namespaces

The reconciler would copy a `kubernetes.io/dockerconfigjson` Secret into each org's workspace namespace
and add it to the sandbox ServiceAccount's `imagePullSecrets`. Rejected because:

- **The first sandbox of every org would fail.** OpenShell does not create the namespace or the
  ServiceAccount at `CreateWorkspace`; it creates them inside the first `CreateSandbox`. The ServiceAccount's
  pull secrets are copied into a pod only when the pod is created, and a pod's `imagePullSecrets` is
  immutable, so the first pods would sit in ImagePullBackOff until the sandbox was deleted and recreated.
  Avoiding it means pre-creating the namespace with OpenShell's labels, naming and ServiceAccount, which
  couples Koinon to OpenShell's internals.
- **It needs cluster-wide Secret rights for the reconciler**: `create`, `update` and `delete` on Secrets in
  every namespace (RBAC cannot scope `create` by name or select namespaces by label), the first such rights
  it would hold.
- **It opens an admission hole.** The sandbox admission policy lets only the OpenShell gateway write Secrets
  in sandbox namespaces, and the escape suite asserts that the reconciler is denied. Allowing the reconciler
  for one Secret name, plus ServiceAccount updates that touch only `imagePullSecrets`, needs a new policy
  and more escape cases, all for something OpenShell already does.
- **Nothing deletes the copy.** Koinon never deletes a workspace, so copies would outlive their org. A
  Koinon-owned deletion path would be new surface; OpenShell's copies are owned by the Sandbox and go away
  with it.

## Consequences

- An operator sets pull secrets in two places, the Koinon release and the OpenShell release (with the
  Secret in OpenShell's namespace). The preflight points at the second when it is missing.
- The token behind a pull Secret is read by the kubelet from the sandbox namespace; the sandbox does not see
  it (no ServiceAccount token, no route to the API server). It should be read-only and scoped to the images
  sandboxes need.
- `sandboxPolicy.supervisorImages` defaults to empty. Anyone who relied on the old literal default gets the
  same value.
- The preflight ClusterRole gains `configmaps get` on one name (`preflight.openshell.configMap`). It is a
  ClusterRole because the ConfigMap lives in OpenShell's namespace, which a Role in a hook could not target
  before that namespace exists. The preflight only warns about the gateway's image settings, since a gateway
  environment variable can override its configuration file. Because the rule is in a ClusterRole, the hook's
  ServiceAccount can read a ConfigMap of that name in any namespace; that is acceptable for a short-lived hook.
- If a future OpenShell drops `sandboxImagePullSecrets`, this decision is revisited; the Koinon-copies option
  above is the fallback.
