# 0106. An install mark decides which OpenShell objects an install owns

- Status: Accepted
- Date: 2026-11-10
- Relates to: [0002](0002-namespace-per-org-isolation.md), [0051](0051-session-home-generations.md), [0045](0045-openshell-hardening-defaults.md), [0046](0046-adr-conventions.md)
- Issues: #461, #275 (the restore drill), #511 (the unmarked list)

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
   profile) says how many objects are still unmarked. It counts from the gateway at most once per resync interval and not on every wake-up (the
   sandboxes and providers across workspaces, the workspace-scoped profiles of the install's own
   workspaces), not from the objects a pass touched, so
   held-back, failing, ephemeral and archived `retained` objects are counted. If the list fails the gauge
   keeps its last values and `koinon_openshell_unmarked_objects_stale` is 1 (it is also 1 before the first
   count).
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
  when every sandbox, provider and provider profile of the live install without a known owner carries the
  mark: the gauge `koinon_openshell_unmarked_objects` is 0 for every kind and
  `koinon_openshell_unmarked_objects_stale` is 0. 0 means no object without a known owner is left; that is
  reached only when the install has no orphans (`org-not-in-db`, `not-reconciler-made`). `waits-for-agent`
  objects (amendment for issue 511: a retained (archived) sandbox and the stopped sandbox of a draft agent,
  with the providers it attaches) are listed and counted apart, in
  `koinon_openshell_unmarked_objects_waiting_for_agent{kind}`, and do not block: a restored copy of the same
  database sees the same agent in the same status and does not claim the object either. They are claimed only
  when the agent changes status. The exception is an agent that was active in the backup and has since
  returned to draft or been archived on the live install: its objects are hidden as `waits-for-agent` on the
  live install, so the gate passes, but the copy sees the agent active and an unmarked sandbox, and
  `reconcileActive` there recreates it with the copy's mark, which deletes the live home volume. (The reverse,
  an agent activated or unarchived since the backup, is safe: its objects are counted and block the gate.) So
  the drill takes its backup only after the gate reads 0 (both gauges as above), and no agent leaves active
  on the live install between the backup and the end of the drill; if one does, the operator does not
  release the copy's hold and takes the backup again once the gate reads 0. The restore hold (ADR 0107) does not
  close this by itself: a held reconciler converges no sandbox, so nothing happens while the copy is held,
  but a drill copy released on the same gateway runs `reconcileActive` like any install. The drill does not
  activate or unarchive agents on the copy, and an operator who wants the second gauge at 0 too unarchives
  the archived agent on the live install, activates or archives the draft agent, or deletes the sandbox when
  its home volume is not needed.
- A foreign-owned agent cannot run on that gateway. A drill that needs running sandboxes needs its own gateway.
- Exec, upload and stop check the sandbox first, which costs one extra `GetSandbox` RPC per call.
- Provider deletion of an attached provider is assumed to fail on real OpenShell; the fake gateway refuses it
  too. If OpenShell allows it, the claim order stays correct and the assumption only costs a clearer error.

## Amendment (issue 511): draft and archived agents, and the unmarked list

On the lab install the gauge stayed flat at profile=28, provider=62, sandbox=1 and nothing said what the
objects were. Decision 5's claim ran only for active agents, so three groups were never claimed:

- **Draft agents.** `reconcileDraft` now claims the agent's gateway-key provider and flavor profile. The
  provider is ensured with the key the agent kept (a draft agent keeps its key for a quick re-activation;
  if the key Secret no longer matches the recorded key nothing is touched and no key is issued), which
  imports the flavor profile with the mark; without the key the org pass below marks the profile. The provider record itself is recreated with the mark only
  when no sandbox attaches it (the stopped sandbox of a draft agent keeps it attached, and decision 5's
  rule about attached providers holds); that record is claimed when the agent is active again. With no
  sandbox the agent's GitHub provider and profile are dropped, because `reconcileActive` makes them again,
  marked, before it creates a sandbox.
- **Flavor profiles no active agent uses are marked, not deleted.** `koinon-agentgateway-<flavor>` belongs to
  the workspace and the flavor, not to the agent: every agent of that flavor in the org shares it, and
  OpenShell refuses to delete a profile a provider uses. An archived agent also leaves the reconcile list
  once its cleanup ends, and agents archived before the mark was on are already gone from it, so the claim
  is made per org, not per archived agent: in the org pass (`claimFlavorProfiles`) every configured flavor
  that no active agent of the org keeps current has its existing profile marked. Active agents re-import
  the profile with the mark on every pass. A draft agent does not count as keeping it: its claim imports
  the profile only while its key Secret matches, and a draft agent without a sandbox row is not reconciled
  at all. The profile needs no key, so the org pass marks it either way. A profile found marked or missing
  is remembered per workspace and not asked for again until an active agent of the flavor imports it, so
  an idle org costs no gateway call per flavor and pass. A failed claim counts as a `sandbox` pass error,
  the kind of the provider record and profile an agent's sandbox converges. The mark is the whole claim:
  archiving keeps the org's data and must not remove what a sibling agent or an un-archive needs. A profile
  that does not exist is not created, and an org without a recorded workspace is skipped. A retained sandbox
  of an archived agent is still claimed only when it is recreated (see the consequences above).
- **Workspaces of orgs that are not in this database, objects the reconciler did not make.** Nothing
  claims these, by design: they are not this install's to change. They are reported instead.

`koinon-reconciler unmarked list [--json]` lists the unmarked objects, one per line: workspace, kind, name
and reason. The gauges count this list, by kind: `koinon_openshell_unmarked_objects` every line except
`waits-for-agent`, `koinon_openshell_unmarked_objects_waiting_for_agent` the `waits-for-agent` lines (a fixed
second series, not a reason label; both read the same list in the same pass and share the stale gauge). It
only reads. An object that carries another install's mark is never listed. The reasons are fixed:

| Reason | Meaning |
|--------|---------|
| `pending-claim` | An active agent's object, or the profile of a configured flavor no agent of the org has. Its next reconcile claims it. |
| `draft-agent` | A draft agent's object, or a flavor profile only draft and archived agents use, that the reconciler claims on its next pass (a provider with no sandbox, the profile). If it is still listed after a pass, the agent's key Secret no longer matches its recorded key (the database cannot show that) and nothing is touched: activate or archive the agent. |
| `archived-agent` | An archived agent's object, or a flavor profile only archived agents use. The org pass marks the profile; the agent's own provider and sandbox are deleted by its cleanup. |
| `waits-for-agent` | The reconciler cannot claim it while the agent stays in its status: a draft agent's stopped sandbox and the providers that sandbox attaches (claimed when the agent is activated, removed when it is archived), and an archived agent's retained sandbox (claimed when the agent is unarchived). |
| `org-not-in-db` | In a workspace no org of this database owns (an org that is deleted counts). Nothing of this install claims it. On a gateway shared with another install, the line can belong to that install, which has not claimed its objects yet (its workspace carries no mark yet): do not delete it, check with that install first. |
| `not-reconciler-made` | Not made by the reconciler, or this database has no record of it. Nothing claims it. |

An operator reads the first gauge as before. A kind above 0 with only `pending-claim`, `draft-agent` and
`archived-agent` lines is still being claimed (it clears within a few passes), unless a `draft-agent` line stays
after a pass (the key Secret case above). Three kinds of line do not clear by themselves and need the
operator: `waits-for-agent` (activate or archive the draft agent; unarchive the archived agent, or delete its
retained sandbox by hand once its home is no longer wanted), `org-not-in-db` (delete the object on the
gateway by hand if it is a leftover of an earlier install or an e2e run; first check it is not another
install's on a shared gateway) and `not-reconciler-made` (delete or leave). `waits-for-agent` lines are in
the second gauge only, so the first reaches 0 on an install with no orphans, whatever its draft and archived
agents hold. The command runs in the reconciler pod
(`kubectl exec deploy/<release>-reconciler -- koinon-reconciler unmarked list`) and needs the settings the
Deployment already gives it: the database URL, the OpenShell address and TLS directory, and the install
mark. Removing the orphans (`--delete-orphans`) and a gauge labeled by reason (ADR 0023 would fix the label
values above) are not built; the operator deletes an orphan on the gateway by hand until they are. The
provider claim (delete by name, then create) has no precondition, because OpenShell's delete request has no
version field; that window is the one decision 5 already documents.

## Links

- Issue #461 (the drill adopted the live install's objects), #275 (the restore drill)
- `internal/execution/openshell/owner.go`, `internal/reconciler/reconciler.go`
