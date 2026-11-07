# Step 2: credentials

Run 2026-10-21. Setup: `hack/spike/gitea-setup.sh` (org `koinon-spike`, repo `hello` with protected
`main`, restricted non-admin user `koinon-spike-bot` with write on that repo only; its 40-hex token
goes straight into OpenBao at `secret/koinon-spike/gitea-bot`). Profile
`hack/spike/profiles/gitea-spike.yaml` binds `SPIKE_TOKEN` to `git.example.invalid:443`
`/api/v1/**`, read-only, `enforcement: enforce`, for `/usr/bin/curl`, `/usr/bin/git`,
`/usr/bin/python3.12`. Provider `gitea-bot` was created from the OpenBao value through the CLI's
environment (`--credential SPIKE_TOKEN`, never in argv). Probe sandbox `p1` runs
`registry.example.invalid/koinon/spike-probe:1` (`hack/spike/probe/`).

The probe (`hack/spike/probe/cred_probe.py`) never knows the real value: it hashes every 40-hex string
(and base64 that decodes to one) it can find; the hashes are compared outside the sandbox.

| # | Result | Evidence |
| --- | --- | --- |
| 2.1 | pass | `SPIKE_TOKEN=openshell:resolve:env:v1104…_SPIKE_TOKEN`; real token's hash not among the 49 40-hex strings found in env and `/proc/*/environ` |
| 2.2 | pass | 9,685 readable files scanned (Landlock allows listing only `/bin /etc /lib /lib64 /sandbox /sbin /tmp /usr`); no hit, plain or base64 |
| 2.3 | pass | the probe sent the placeholder to Gitea (`200`, login `koinon-spike-bot`) and then scanned its own memory: no hit. Other processes' memory and environ (`openshell-sandbox` PID 1, `sleep`) are `Permission denied` despite the same UID |
| 2.4 | pass | the supervisor runs in a separate pod (`os-supervisor-<id>`), outside the Kata VM; no supervisor process is visible from the agent; the in-VM PID 1 is unreadable |
| 2.5 | pass | placeholder → Gitea `/api/v1/user`: `200`. → `whoami` (network allowed by `policies/step2-echo.yaml`): `403 credential_endpoint_mismatch`, not forwarded; same request without placeholder: `200`. Gitea's side (`raw/2.5-gitea-side.md`): the placeholder request returns the private `koinon-spike/hello`, an anonymous one gets `403 Only signed in user is allowed`. Gitea's log has no user or token and shows the ingress hop, not the sandbox, as source. Placeholder in a query to `whoami`: `403`. Gitea outside `/api/v1`: `403 policy_denied`. `POST` to the read-only API: `403`. `perl` (not an allowed binary): socket refused |
| 2.6 | pass | gateway, both supervisor pods, workload pods, Agent Sandbox controller, namespace events and object dumps: 0 occurrences of the token or its base64. All Kubernetes Secrets in `openshell` and the workspace namespace: clean |
| 2.7 | pass, conditional on `stable_placeholder: true` | token rotated in Gitea and OpenBao, pushed with `provider update --wait` (applied in 3 s), no sandbox restart (restart count 0). Default revision-scoped placeholders: new processes get the new value, but an old placeholder keeps resolving the **old** value (Gitea `401` after revocation). With `stable_placeholder: true` on the profile credential, a placeholder taken before rotation resolves the new value afterwards (`200`) |
| 2.8 | confirmed | `sandbox create --env KEY=value` puts the plain value in the agent's environment (the CLI only warns for credential-looking values); `--env-from` likewise. Neither shows in the `Sandbox` object; the gateway stores them |

## Findings

1. **OpenShell stores its own copy of every credential.** "Rotating in OpenBao" means updating the
   source and pushing with `provider update`; OpenShell does not watch OpenBao. This matches ADR
   0043's push model (Koinon mints or reads, then pushes).
2. **Koinon must set `stable_placeholder: true`** on every credential it rotates (decided 2026-10-21: added to the ADR 0045 hardening defaults through ADR 0048). Without it,
   long-running harness processes keep the revision they started with and fail once the old value
   is revoked or expires. Gateway-managed refresh (OAuth2, STS) has its own continuity.
3. **Landlock is `best_effort` by default** (`landlock.compatibility`); ADR 0045 should require the
   strict setting if the schema offers one (check in step 3).
4. **Every sandbox's supervisor Secret holds its interception CA key** (`proxy-ca.key`) in the
   workspace namespace. The workload pod does not mount it; step 8 checks it stays unreachable.
5. The CLI and API never print credential values (`provider get` shows key names only).
6. Agent-authored policy proposals have an `--approval-mode` (default `manual`); relevant to step 6.
