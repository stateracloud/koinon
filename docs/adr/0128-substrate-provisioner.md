---
status: accepted
date: 2026-11-26
area: runtime-sandbox
tags: [sandbox, security, identity, kubernetes]
relates: ["0002", "0075", "0119", "0120"]
---
# 0128. A provisioner holds the global Substrate role

- Status: Accepted (decided by the maintainer on 2026-11-26)
- Date: 2026-11-26
- Area: runtime-sandbox
- Tags: sandbox, security, identity, kubernetes
- Relates to: [0119](0119-agent-substrate-execution-layer.md) (this ADR sets the principals of its Substrate driver), [0002](0002-namespace-per-org-isolation.md), [0075](0075-isolation-tiers.md), [0120](0120-execution-hardening-defaults.md)
- Issue: #1398

## Context

The Substrate driver of the reconciler has no role in Substrate's API. The first task of the Substrate
kind leg fails with `PermissionDenied: principal "user:system%3Aserviceaccount%3Akoinon%3Akoinon-reconciler"
lacks "can_get" on "atespace:kw-..."`. Every real install has this error.

The cause is in Substrate v1.0.0 (the pin of Koinon):

- Only the global role `owner` can create an atespace (`cmd/ateapi/internal/authz/model.fga:56`,
  `registry.go:167`). No narrower role can, and no scope by name prefix exists.
- Creating an atespace gives the caller no role on it (`controlapi/atespace.go:49-63`).
- The install gives only the default owner `system:serviceaccount:ate-system:ate-client` a role.
- A write of an access policy replaces the whole policy (`authz/policy_manager.go:118-150`).
- A caller with no role gets `PermissionDenied`, not `NotFound`, for an atespace that does not exist.

Koinon makes one atespace for each org (ADR 0002), once, and never deletes it (`internal/execution/names.go`,
`internal/reconciler/reconciler.go`). So the creation is rare. The reconciler needs `editor` in the atespaces of
its orgs. Agentd calls only `Exec` and `Upload`, which read the actor. It never suspends or resumes, so it needs
`viewer` only.

The simple fix gives the reconciler the global `owner` role. Then a compromise of the reconciler (a large
component that reads run state and drives every sandbox) gives full Substrate admin: delete every atespace,
rewrite the global policy, and control the atespaces of another product on a shared Substrate (ADR 0119 D4).

## Prior art

- Substrate's own guide (`docs/guides/authorization.md`) says to give day-to-day principals `owner` in the global
  policy, where it can be revoked, and not to make them bootstrap owners. This ADR follows it.
- Kubernetes splits a privileged task from a large controller by ServiceAccount. A small component that holds the
  right to grant, and takes no untrusted input, is the usual answer when the platform has no scoped create right.
- No upstream work on a scoped create right was found: PR 3527 (an operator role) does not help with creation.
  The research report is `2026-11-26-substrate-authz` of the koinon-research workflow.

## Decision

1. **A provisioner holds the global `owner` role.** It is a small Deployment with its own ServiceAccount
   (`koinon-provisioner`). It does one job: it creates the atespace of an org and writes the access policy of
   that atespace. No other Koinon component holds a global role in Substrate.
2. **The principals.**

   | Component | Principal | Role |
   |---|---|---|
   | provisioner | `system:serviceaccount:<ns>:koinon-provisioner` | `owner` in the global policy |
   | reconciler | `system:serviceaccount:<ns>:koinon-reconciler` | `editor` in each org atespace |
   | agentd | `system:serviceaccount:<ns>:koinon-agentd` | `viewer` in each org atespace |

   The provisioner owns the whole policy of an org atespace, so a full replacement is safe. It writes exactly
   these two bindings and so also removes a member that someone added.
3. **The provisioner is a mode of the reconciler binary** (`koinon-reconciler provision`). It needs no new image,
   no release job and no database credential. It runs in its own pod with its own token, so the reconciler pod
   never holds the provisioner's token. Its code lives in `internal/substrateauthz`.
4. **The interface.** The reconciler asks over HTTP: `POST /v1/atespaces` with `{"name": "kw-<16 hex>"}`.
   - The name must match `kw-` and 16 lower-case hex characters, the form of `execution.WorkspaceName`. The
     provisioner touches no other atespace.
   - The caller sends a projected ServiceAccount token with the audience `koinon-provisioner`. The provisioner
     checks it with a TokenReview and allows only the reconciler's username. A valid token of another component
     gets 403. A NetworkPolicy also admits only the reconciler pods to the port.
   - The call is idempotent: it creates the atespace when it is missing, and it writes the policy when that is
     missing or differs. It repairs drift and adopts an atespace that exists from before the provisioner.
   - Failure: 400 for a bad name, 401 and 403 for the caller, 502 when Substrate refuses or is not reachable.
     The driver fails `EnsureWorkspace` with `ErrUnavailable`, and the reconciler retries on its next pass, as for
     any other prerequisite. The provisioner keeps no state.
5. **When the reconciler asks.** `EnsureWorkspace` reads the atespace first. Only when that returns `NotFound` or
   `PermissionDenied` does the driver ask the provisioner, then read again. A running install thus makes no call
   to the provisioner. With a provisioner set, the driver never calls `CreateAtespace`.
6. **The installer adds the provisioner to the global policy** (`koinon-api substrate install`), as a revocable
   member and never as a bootstrap owner. It reads the policy, adds the member and writes it with the uid and the
   version it read. It keeps every other member. It creates the policy when there is none. A change in between
   fails the write, and the step reads again (up to 5 times). It writes nothing when the grant exists.
7. **Upgrade** runs the same installer step. An upgrade of an existing install thus adds the grant and keeps the
   other members. The atespaces that exist from before get their policy at the next ask of the reconciler: the
   first read as the reconciler gives `PermissionDenied`, and the provisioner then writes the policy.
8. **The kind leg and CI** get the grant from the same installer call. They add no other component to the global
   policy.
9. **The installer's identity.** The installer acts as `ate-client`, the default bootstrap owner of the install:
   it mints a short-lived token for that ServiceAccount (audience `api.ate-system.svc`) and calls ateapi through a
   port-forward to the api server pod. The installer checks the api server's certificate before it sends the token:
   the CA is the ClusterTrustBundle of Substrate's servicedns signer (`servicedns.podcert.ate.dev/identity`, label
   `podcert.ate.dev/canarying=live`), and the name checked is `api.<namespace>.svc`. `--api-ca-file` replaces the
   bundle. No CA is an error: the installer never skips the check. An install whose bootstrap owners are another
   principal passes a token file instead (`--api-token-file`).

## Threat model

- **Who attacks:** a compromised reconciler or agentd pod, or another pod in the cluster.
- **What they reach:** the reconciler can use the provisioner's interface, but only for names of the form
  `kw-<hex>`, which only create an empty atespace with Koinon's policy. Agentd and any other pod are refused
  (403) and cannot reach the port (NetworkPolicy). Neither holds a Substrate global role: they cannot create or
  delete an atespace, change a policy, or touch an atespace of another product.
- **The provisioner's own exposure:** it takes no model traffic and no agent output. Its only input is a name that
  it validates. It holds the global `owner` token, projected for the audience of Substrate's api and mounted only
  in its pod.
- **What is left:** the provisioner can still delete or rewrite anything in Substrate if it is compromised. The
  traffic to the provisioner is plain HTTP inside the cluster: a projected token that is replayed works only
  against the provisioner, for the reconciler's name, and the call only creates an atespace. The installer's
  port-forward verifies the api server certificate. The token that the installer mints lives for ten minutes. A token from `--api-token-file` has the life that its issuer gave it.

## Consequences

- One more pod, ServiceAccount, ClusterRole (TokenReview), Service and NetworkPolicy in the chart.
- The reconciler no longer needs, and no longer gets, a Substrate global role.
- Exit condition: Substrate has no scoped create role and no owner grant for the creator. When a Substrate
  release adds one, check whether the provisioner can drop to it or be removed. The gap is recorded here and in
  #1398. Nobody files it upstream (a human may raise it in their own words).
- Atespace names stay stable for each org and are never reused. If Koinon ever deletes an org atespace, read
  upstream issue 3255 (a re-created name inherits grants) first.
