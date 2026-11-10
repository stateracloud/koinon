# 0106. An install mark decides which OpenShell objects an install owns

- Status: Proposed
- Date: 2026-11-10
- Relates to: [0002](0002-namespace-per-org-isolation.md), [0051](0051-session-home-generations.md), [0045](0045-openshell-hardening-defaults.md), [0046](0046-adr-conventions.md)
- Issues: #461, #275 (the restore drill)

## Context

Koinon names its OpenShell objects (workspace, sandbox, provider, provider profile) from the org and agent
IDs only. Those IDs live in Postgres, so a restored database computes the same names as the install it was
dumped from. The restore drill (#275) and any restore next to the live install on one gateway therefore
hit the same objects: the second install adopted the first's sandbox, stopped it, recreated it with its own
spec and deleted its providers (#461).

Names cannot tell two installs apart, because everything that could go into a name travels with the
database: the org and agent IDs, the recorded sandbox names, and `install_identity` (the install ID that
binds a database to a key Secret). A namespace name is not unique either: a restore can use the same
namespace on another cluster that shares the gateway, or a different namespace on the same cluster.

## Decision

1. **A random install mark.** The chart's ensure-secrets hook Job makes the Secret `koinon-install-mark`
   once, with a random `token` (`installMark.existingSecret` names a Secret the operator manages). The
   token is not in Postgres, so a restored database does not carry it. It survives `helm uninstall` and
   upgrades.
2. **Objects carry a hash of it.** The OpenShell backend stamps `koinon.dev/install` (a label on sandboxes,
   providers and workspaces, an annotation on profiles) with a hash of the token. The token itself never
   leaves the Secret.
3. **A foreign mark is refused.** Adopting, changing, starting, stopping, exec-ing in, uploading to or
   deleting an object whose mark differs fails with `ErrForeign`. The reconciler records the agent row as
   `error` with reason `OwnedByAnotherInstall`; `koinon-agentd` fails the runs of that agent and does not run
   them in the other install's sandbox.
4. **Unmarked means own.** An object without a mark was made before this ADR. The install treats it as its
   own and the upgrade itself recreates nothing at once; decision 5 says how and when it marks them.
5. **The live install claims legacy objects when it recreates them.** OpenShell keeps labels immutable. An
   unmarked sandbox is recreated with the mark like any spec change (never while a run holds the agent, and
   within the per-pass recreate cap, ADR 0085). An unmarked provider is deleted and made again with the mark
   only on a pass with no sandbox, because a provider that a live sandbox attaches must not be removed.
   An attached provider is therefore claimed only on the pass after its sandbox is gone. Old workspaces stay
   unmarked: they are shared, idempotent records. New workspaces are stamped and checked. Profiles are
   re-imported with the mark (it is part of their spec hash). The claim takes several passes, longer while
   runs hold agents; the reconciler gauge `koinon_openshell_unmarked_objects{kind}` (sandbox, provider,
   profile) says how many objects it still saw unmarked in the last pass.
6. **Names do not change.** No sandbox is recreated only because of the names.

## Alternatives considered

- **Put an install ID in the names.** The ID is in Postgres, so a restore has the same one; a new ID for
  a restore would break the binding to the key Secret. It would also rename every object on upgrade and
  recreate the whole fleet. Decision 5 has the same cost: every unmarked sandbox is recreated once and its
  home volume replaced, only spread over passes by the held-run rule and the cap, and not renamed.
- **Install ID plus a hash of the namespace.** Two installs can share a namespace name (two clusters, one
  gateway), and a namespace move would change the owner of a healthy install. This was the first version of
  this change; review round 1 rejected it.
- **An adopt override flag** (a switch that lets a restored install take over foreign objects). It is the
  right tool for a deliberate takeover, but as the only mechanism it turns the safe default into a
  setting that someone forgets. Restoring the mark Secret does the same job and is a visible, recorded step.

## Prior art

Kubernetes owner references and the `app.kubernetes.io/instance` label, Terraform's state lock and Crossplane's
external-name annotation all separate "mine" from "the same name" with a value that is not derived from the
name. Velero's restore marks restored objects the same way.

## Consequences

- A lost Secret (a deleted namespace) means the install no longer owns its objects. Recover by applying
  the backed-up Secret (see [Back up and restore](../site/src/content/docs/guides/backup-restore.md)),
  by creating it again with the old `token`, or by deleting the install's objects on the gateway so that the
  reconciler makes them again with a new mark.
- Moving an install to another namespace keeps its objects when the mark Secret moves with it. A restore that
  replaces the old install applies the mark backup before its first start; a drill copy does not.
- Legacy objects stay open to a second install until the live install has recreated them, which takes
  several passes (held runs, the cap, attached providers). A restored copy may run on the same gateway only
  when every sandbox, provider and provider profile of the live install carries the mark: the gauge
  `koinon_openshell_unmarked_objects` is 0 for every kind.
- A foreign-owned agent cannot run on that gateway. A drill that needs running sandboxes needs its own gateway.
- Exec, upload and stop check the sandbox first, which costs one extra `GetSandbox` RPC per call.
- Provider deletion of an attached provider is assumed to fail on real OpenShell; the fake gateway refuses it
  too. If OpenShell allows it, the claim order stays correct and the assumption only costs a clearer error.

## Links

- Issue #461 (the drill adopted the live install's objects), #275 (the restore drill)
- `internal/execution/openshell/owner.go`, `internal/reconciler/reconciler.go`
