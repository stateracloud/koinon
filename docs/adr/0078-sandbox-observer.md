# 0078. Sandbox observer: get-only reads of sandbox pods to explain why a sandbox never became ready

- Status: Accepted (delivery in rc.7)
- Date: 2026-10-29
- Amends: [0048](0048-openshell-spike-results.md) (decision 3 and the 2026-10-26 amendment: the reconciler
  holds no access to Agent Sandbox objects), [0066](0066-triggers-and-scheduling.md) (the wait cause set and
  where its input comes from; amendment 2026-10-28, rc.6). Touches [0045](0045-openshell-hardening-defaults.md)
  and [0075](0075-isolation-tiers.md) only to leave them as they are (decision 8)
- Relates to: [0002](0002-namespace-per-org-isolation.md), [0023](0023-observability-and-audit.md),
  [0061](0061-image-pull-secrets-and-registry-mirrors.md), [#160](https://github.com/stateracloud/koinon/issues/160)
- Pending verification: the home-lab re-run on a managed cluster (the kind e2e proves the mechanism)

## Context

ADR 0066 (rc.6) caps the wait for a sandbox and names the cause when the cap runs out: `image_pull`,
`admission`, `no_nodes` or `unknown`. agentd classifies the reason and message of the first condition
that is not true in OpenShell's sandbox status, which the reconciler records in `agent_sandboxes.reason`
and `message`.

A capture on kind (OpenShell 0.1.5 with Agent Sandbox v1.0.7, 2026-10-29) showed that an image that
cannot be pulled is always `unknown`. OpenShell reports only `ConfigurationPending "Waiting for effective
configuration validation before workload activation"`, and the pod's `ImagePullBackOff` with the full
kubelet error appears only in the pod's container statuses and events. The same goes for a crash loop.
Reading OpenShell's and Agent Sandbox's source (v0.1.5, v0.1.6 and main; Agent Sandbox v1.0.7, v1.0.8 and
main) found two upstream causes, neither fixed in a release:

1. **The gateway overwrites the driver's `Ready` condition.** While the supervisor has not reported its
   configuration, `apply_configuration_readiness` removes the driver's `Ready` condition and pushes
   `Ready=False ConfigurationPending`. A pod that cannot pull its image never starts a supervisor, so the
   rewrite always applies and whatever Agent Sandbox wrote (`DependenciesNotReady`, or a `ReconcilerError`
   with an admission denial) is lost. Other conditions pass through, which is why `Unschedulable` reached
   Koinon.
2. **Managed mode delivers no platform events.** The Kubernetes driver maps `Failed` and `BackOff` events to
   `PlatformEvent`s only in its single-namespace watch. `managed` and `operator` modes use the cluster-wide
   watch, which watches Sandbox objects only, so `WatchSandbox` `follow_events` is always empty in the mode
   Koinon requires (ADR 0002, 0045). `SandboxStatus` has no container statuses, and `GetSandboxLogs`
   returns gateway and supervisor logs only. No OpenShell setting changes either.

Agent Sandbox v1.0.8 appends the waiting reason to the Sandbox's `Ready` message, but the gateway removes
that condition for the same reason as above.

The reason that matters most to operators is the pull error: a mistyped flavor image, a registry that needs
credentials, or a mirror that is down. Today the run fails after the cap with "the cause is unknown".

## Decision

1. **The reconciler reads the pod, with `get` only.** When a sandbox is in phase Provisioning and OpenShell's
   reason is one of `ConfigurationPending`, `DependenciesNotReady` or `SchedulingGated`, the reconciler gets
   the pod named like the sandbox in the workspace's namespace (in managed mode the pod and the Sandbox
   object carry the OpenShell sandbox's name). When the pod does not exist it gets the Sandbox object and
   reads its `Ready=False` condition, which is where Agent Sandbox puts an admission refusal
   (`ReconcilerError "Error seen: pods ... is forbidden: ValidatingAdmissionPolicy ..."`). It never lists or
   watches, and it reads nothing for an informative reason such as `Unschedulable`.
2. **The detail replaces the recorded reason and message.** `execution.PodWaitDetail` takes the first init
   container or container that waits for a reason other than `ContainerCreating` and `PodInitializing`
   (init containers first), with its message cut to 512 bytes; a crash loop carries its last exit.
   `record()` writes it into the existing `agent_sandboxes.reason` and `message` columns. agentd and the
   classifier read the same columns, so no migration and no agentd change are needed. When the pod says
   nothing, OpenShell's words stay. A failed read never fails a reconcile pass: it is counted and logged.
3. **A new wait cause, `crash_loop`.** The classifier maps `CrashLoopBackOff` and `CreateContainerError` (and
   the v1.0.8 text `container "agent" is in CrashLoopBackOff (last exit: Error)`) to it. The hand-written
   causes are now `image_pull`, `admission`, `no_nodes`, `crash_loop` and `unknown`: a fixed set, so the
   metric label set stays fixed (ADR 0023). Web timeline labels and the metrics guide list it.
4. **Access is one RoleBinding per workspace namespace, created by the reconciler.** The chart renders
   (`templates/sandbox-observer.yaml`, names prefixed `<namespace>-<fullname>` like the sandbox policies):
   - the **observer ClusterRole**: `get` on `pods` and on `agents.x-k8s.io` `sandboxes`. It is never bound
     cluster-wide;
   - the **binder ClusterRole**, bound cluster-wide to the reconciler's ServiceAccount: `create` on
     `rolebindings`, and `bind` on `clusterroles` with `resourceNames` the observer ClusterRole;
   - when the reconciler has a workspace that is Active it creates, once per namespace and process, a
     RoleBinding named like the observer ClusterRole, with that ClusterRole as `roleRef` and the reconciler
     as its only subject, in `openshell-<gatewayId>-<workspace>`. `AlreadyExists` counts as done.
     A `Forbidden` on a read drops the cache entry so that the binding is made again.

   There is no list, watch, events, update or delete: the pod name is the sandbox name, the kubelet's waiting
   message already holds the whole pull error, `PodScheduled` already arrives through OpenShell, and the
   namespace and its RoleBinding go when the workspace goes.
5. **A native ValidatingAdmissionPolicy bounds the grant** (never Kyverno).
   RBAC cannot select namespaces by label and `create rolebindings` has to be cluster-wide because workspace
   namespaces appear at runtime, so without the policy the binder could grant pod reads anywhere. The policy
   `<base>-sandbox-observer-binding` (`failurePolicy: Fail`, same `validationActions` as `sandboxPolicy`, denials
   answer 403) matches RoleBinding CREATE and UPDATE in all namespaces. A `matchConditions` expression lets
   only the reconciler's writes, writes under the fixed name, and writes that reference the observer
   ClusterRole reach the validations; every other RoleBinding in the cluster passes untouched. Then:
   - the reconciler may write only in a namespace labelled `openshell.ai/managed-by=openshell` and
     `openshell.ai/gateway-id=<openshell.gatewayId>`, only under the fixed name, only with `roleRef` exactly
     the observer ClusterRole, and only with `subjects` exactly its own ServiceAccount;
   - nobody else may write that name, or any RoleBinding to that ClusterRole, in an OpenShell sandbox
     namespace, cluster admins included (the identity-keyed pattern of `sandbox-policy.yaml`).
6. **The chart refuses to render the observer without the policy.** `koinon.validate` fails when
   `sandboxObserver.enabled` is set and `sandboxPolicy.enabled` is not, or `sandboxPolicy.validationActions` lacks
   `Deny`. The policy is what bounds the grant, so the grant does not exist without it.
7. **On by default, with an opt-out.** `sandboxObserver.enabled: true`. Platform teams that refuse the grant
   set `false`: the chart renders none of the objects, the reconciler reads nothing (and logs it at start),
   the preflight warns, and a pull error stays `unknown`. Andre, 2026-10-29: on by default, shipped in rc.7
   before the home-lab re-run.
8. **The namespace comes from a setting, not from OpenShell.** OpenShell's API does not return a workspace's
   namespace. Koinon uses OpenShell's documented `openshell-{gateway_id}-{workspace}` with the new value
   `openshell.gatewayId` (default `openshell`, the OpenShell chart's default). The preflight reads
   `gateway.toml` from the OpenShell ConfigMap and compares `gateway_id` with it. The key appears twice
   (`[openshell.drivers.kubernetes]` and `[openshell.gateway.gateway_jwt]`), so the parse follows the
   table headers. It also warns when `workspace_mode` is not `managed`. Nothing in ADR 0045 or 0075 changes:
   no new identity writes pods or Sandboxes, reads do not pass through admission, and every tier behaves the same
   (reads do not depend on the runtime class, user namespaces or the dedicated pool).
9. **Metrics.** `koinon_reconciler_sandbox_observer_errors_total{reason}` with `reason` one of `forbidden`,
   `not_found` or `other`. `koinon_agentd_sandbox_wait_cap_total{cause}` gains the value `crash_loop`.
10. **Exit.** This is a workaround for two upstream defects, and it is removed when they are fixed. Koinon
    files both against OpenShell (drafts in the 2026-10-29 research): the cluster-wide watch has no Event
    watcher, and the gateway should keep the driver's message, for example as a separate `WorkloadReady`
    condition, when it writes `ConfigurationPending`. When a pinned OpenShell release has them, Koinon
    classifies the platform events or the preserved message, deletes `internal/reconciler/observer.go`,
    `templates/sandbox-observer.yaml` and the `sandboxObserver` and `openshell.gatewayId` values (the latter
    stays if something else needs it), and this ADR becomes Superseded. A pin bump (ADR 0077) is the trigger
    to check. Agent Sandbox v1.0.8 is not needed for any of this and is not required.

## Consequences

- A missing flavor image, a registry that refuses, a crash loop and an admission refusal each reach the
  run's failure message, the unhealthy notification and the metric with their own cause, with the kubelet's
  text as the detail. rc.6's `unknown` remains for what the pod does not explain.
- **What a compromised reconciler gains:** `get` on pods and Sandbox objects in sandbox namespaces, by name.
  Those pods hold no secrets by rule (AGENTS.md "No secrets in sandboxes"; supervisor pods reference Secret
  volumes but contain no values). The reconciler already holds more: the per-agent gateway keys, the
  provider-key Secret and the OpenShell client certificate.
- **What the grant is not:** it is not `list`, `watch`, `exec`, a Secret read or any write, and it is not in
  Koinon's own namespace or any other. The reconciler cannot read or change a RoleBinding after creating it.
- Koinon's reconciler is now a cluster-scoped actor (ADR 0048's amendment of 2026-10-26 removed an unused
  cluster-wide Sandbox grant; this one is used and narrower). It is one grant for Rivet and platform teams
  to review: the binder ClusterRole and the policy, both in one template.
- A RoleBinding exists in every workspace namespace until the namespace goes; deleting it by hand costs one
  `forbidden` count and is repaired on the next read.
- The policy matches RoleBinding writes cluster-wide with `failurePolicy: Fail`; its `matchConditions` keep
  the CEL work to the reconciler's and observer-role writes. A CEL error would deny those writes only.
- If `openshell.gatewayId` is wrong, the reconciler's RoleBinding is refused by the policy (a `forbidden`
  count), the reads do not happen and causes stay `unknown`; the preflight names the mismatch.
- Costs: one `get` per pass for each sandbox that is provisioning with an opaque reason, bounded by the
  sandbox job limit and a 5 s timeout per call.

## Alternatives

- **O1. OpenShell surfaces it (the exit, decision 10).** Best security: Koinon talks only to OpenShell, no
  RBAC delta. Rejected for rc.7 only because it needs an OpenShell release Koinon does not control, and
  v0.1.6 and main have neither fix (related upstream: #4415 closed into #2675, which covers the CLI
  progress output only).
- **O2. Agent Sandbox surfaces it and Koinon reads Sandbox objects.** v1.0.8 gives the pull reason (no
  kubelet message) and crash loops, v1.0.7 only admission. The access needed is the same grant as the
  chosen option with a different resource, and it needs the pin bump. Folded in: the Sandbox object is the
  fallback when the pod is missing.
- **O4. The chart creates Roles and RoleBindings in known namespaces.** Workspaces are created at runtime,
  one per org, and the chart cannot know them without `lookup` (forbidden by AGENTS.md) or a `helm upgrade`
  per org. Operator mode would need cluster-wide namespace create, which ADR 0002 rejected.
- **O5. A cluster-wide ClusterRole of `get` on pods plus events.** Half a day of work, same knowledge, but
  every pod spec in the cluster, including literal `env` values of unrelated workloads. Reads cannot be
  narrowed by admission. Rejected: least privilege.
- **O6. Other signals.** The flavor-prepull DaemonSet proves nothing about the sandbox's pull (different
  namespace and pull secrets) and needs its own RBAC. Kubelet or node signals need `nodes/proxy`, which is
  node admin. OpenShell's validation webhook runs before the pod exists. A registry HEAD from Koinon takes
  another network path with other credentials. All rejected.
- **A RoleBinding per namespace without the policy.** Rejected: the binder could then grant pod reads
  anywhere (decision 5).
- **Do nothing and document `unknown`.** The cause the operator most needs stays hidden until OpenShell
  ships a fix.

## Upstream verification

Read from clones of NVIDIA/OpenShell (v0.1.5, v0.1.6, main d4e32263) and kubernetes-sigs/agent-sandbox
(v1.0.7, v1.0.8, main b127767), and from a capture on kind `koinon-e1` (test fixtures:
`internal/execution/testdata`):

- `apply_configuration_readiness` (`crates/openshell-server/src/compute/mod.rs`) removes the driver's `Ready`
  condition (`condition.r#type != "Ready"`); same in v0.1.6 and main.
- The Event mapping (`map_kube_event_to_platform`, `driver.rs`) runs only in `watch_sandboxes_single_namespace`;
  `cluster_wide_watch_stream` has no Event watcher; same in v0.1.6 and main.
- Agent Sandbox v1.0.8 `podContainerIssue` (`controllers/sandbox_controller.go`) appends the waiting reason to
  the Ready message; v1.0.7 reports `Pod exists with phase: Pending`.
- The namespace `openshell-openshell-kw-ff625e7f210d54d1` carries `openshell.ai/managed-by`,
  `openshell.ai/gateway-id` and `openshell.ai/sandbox-workspace`; the pod and the Sandbox are named after the
  OpenShell sandbox.
- Not captured: the exact `ReconcilerError` text. The tests build it from Agent Sandbox's format string and
  from the captured policy denial, and say so.

## Links

- [0048](0048-openshell-spike-results.md), [0066](0066-triggers-and-scheduling.md), [0045](0045-openshell-hardening-defaults.md),
  [0075](0075-isolation-tiers.md), [0077](0077-pinned-openshell-and-installer.md)
- Chart: `charts/koinon/templates/sandbox-observer.yaml`. Code: `internal/execution/podcause.go`,
  `internal/reconciler/observer.go`. Tests: `test/escape/admission_observer_test.go`.
