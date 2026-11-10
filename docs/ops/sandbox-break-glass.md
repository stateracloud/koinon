# Sandbox break-glass: getting into a sandbox pod

Koinon denies `kubectl exec`, `attach`, `port-forward` and the API server's `pods/proxy` and
`services/proxy` into OpenShell sandbox namespaces, for every identity, cluster admins included
([ADR 0048](../adr/0048-openshell-spike-results.md) decision 3). A process started that way runs
outside the OpenShell supervisor's Landlock and seccomp policy and its OCSF audit, so it would
defeat the boundary the sandbox exists for. The chart enforces this with the
ValidatingAdmissionPolicy `<namespace>-<fullname>-sandbox-connect` (for a release `koinon` in
namespace `koinon`: `koinon-koinon-sandbox-connect`) and its binding of the same name, on every
namespace labelled `openshell.ai/managed-by=openshell` and `openshell.ai/gateway-id=<openshell.gatewayId>`. Neither the OpenShell gateway nor the Agent
Sandbox controller uses these calls, so nobody is exempt.

This runbook is for the rare case where that is not enough. It is not an exemption: it is a
deliberate, audited switch that a cluster admin flips and flips back.

## When it is justified

Only when both are true:

- the normal tools below cannot answer the question, and
- either the sandbox's supervisor is dead, so OpenShell exec fails and logs and a recreate do not
  help, or a security incident needs live inspection of a running sandbox.

## Try these first

1. OpenShell exec (`openshell sandbox exec`, or the agent's own tools). It runs under the
   supervisor's policy and is audited.
2. `kubectl -n <workspace-namespace> logs <pod> --all-containers`. Logs are a GET on `pods/log`,
   which the policy does not touch.
3. The agent run's events and the audit trail in Koinon (the run page, or the OCSF log the
   supervisor emits).
4. Recreate the sandbox. Koinon does this automatically when a sandbox is lost (since
   old issue 102); the home volume persists.
5. Saved work. A stuck sandbox's uncommitted work is saved by `koinon-agentd` before it is
   recreated ([ADR 0054](../adr/0054-unkillable-sandbox-process.md)).

## Forensics without exec

You rarely need a shell in the live pod. Inspect a copy instead:

1. Stop the sandbox (delete the Koinon run, or scale the OpenShell sandbox to zero) so nothing
   writes to the home volume.
2. Snapshot the sandbox's home PVC. If the CSI driver supports it:

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshot
   metadata: {name: forensics-<id>, namespace: <workspace-namespace>}
   spec:
     source: {persistentVolumeClaimName: <home-pvc>}
   ```

   Restore it into a PVC in a namespace that is **not** an OpenShell workspace namespace, mount
   that in a pod there, and inspect the copy with whatever tools you like.
3. Node-level access (`crictl`, `ctr`, the node's shell) is outside admission control entirely.
   It is a node-admin action: treat it as break-glass too and record it.

## The procedure

Only a cluster admin does this. Suspend **only** the CONNECT binding; the pod and object policies
stay on.

1. Tell the incident channel who is doing it and why. Note the time.
2. Switch the binding to audit-only. The connections are allowed. Each one is recorded with the
   policy's message in the API server audit log only if the cluster has an audit policy at
   Metadata level or higher for these requests; otherwise `Audit` just allows them, so check that
   first:

   ```sh
   kubectl patch validatingadmissionpolicybinding koinon-koinon-sandbox-connect \
     --type merge -p '{"spec":{"validationActions":["Audit"]}}'
   ```

   Use your release's names: `<namespace>-<fullname>-sandbox-connect`. Or delete the binding,
   which allows the connections without the audit annotation:

   ```sh
   kubectl delete validatingadmissionpolicybinding koinon-koinon-sandbox-connect
   ```

   Policy changes take a few seconds to apply.
3. Do the minimum, for example `kubectl -n <workspace-namespace> exec -it <pod> -- sh`. Nothing
   here runs under the supervisor's policy: do not copy secrets out and do not leave anything
   running.
4. Restore the binding straight away. Either re-apply the chart (`helm upgrade` with the same
   values; an Argo CD or Flux sync does the same), or by hand:

   ```sh
   kubectl patch validatingadmissionpolicybinding koinon-koinon-sandbox-connect \
     --type merge -p '{"spec":{"validationActions":["Deny"]}}'
   ```

   If you deleted it, `helm upgrade` (or a GitOps sync) recreates it; by hand, render it with
   `helm template <release> charts/koinon -n <namespace> -f <your values> --show-only templates/sandbox-policy.yaml`
   and apply the `...-sandbox-connect` binding. Use the same value as `sandboxPolicy.validationActions`
   (default `[Deny]`). Without `-f <your values>` a non-default `validationActions` or
   `namespaceSelector` is lost.
5. Check it is back: `kubectl -n <workspace-namespace> exec <pod> -- true` must be denied with
   "use OpenShell exec and logs".
6. Record the incident: who, when, why, which pod, what was run, and what you changed in the
   sandbox. The sandbox you opened is no longer trustworthy: delete and recreate it.

## What leaves a trace

- The Kubernetes audit log records the `patch` or `delete` of the binding and every exec, attach
  and port-forward made while it was open (with the `Audit` action, also the policy's
  `validation.policy.admission.k8s.io` annotation).
- Under Argo CD the changed binding shows as drift until the next sync; Flux shows it only with
  helm-controller `driftDetection.mode: enabled`. A self-heal
  policy puts it back by itself. That is the intended safety net; turn self-heal off for the
  binding only if you need the window to last longer, and turn it back on.

## Keep the boundary intact

- Do not label Koinon's namespace or the OpenShell gateway's namespace with
  `openshell.ai/managed-by=openshell`. The policies would then cover them and block exec into
  those pods. OpenShell labels only its workspace namespaces.
- Do not grant RBAC on `nodes/proxy`. It reaches the kubelet's exec API directly, bypassing
  admission just like node access does, so it is a way around this policy.
