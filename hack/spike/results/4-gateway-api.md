# Step 4: gateway API from Go

Run 2026-10-21 with `hack/spike/gatewayctl` (its own module, so the spike dependency stays out of
Koinon's root module). SDK: `github.com/NVIDIA/OpenShell/sdk/go`, pinned by pseudo-version to the
v0.1.5 commit `bdb956f` because the SDK has no `sdk/go/*` tags. Transport: mTLS client bundle over
the port-forward (`127.0.0.1:18080`; the server certificate covers `127.0.0.1`).

| # | Result | Evidence |
| --- | --- | --- |
| 4.1 | pass | `CreateSandboxFromTemplate` → `WaitReady` (23 s) → `Stop`/`WaitStopped` → `Start`/`WaitReady` → `Delete`; `go run . lifecycle` |
| 4.2 | partial | `SandboxTemplates().Create` with image and resources; a sandbox from it gets the probe image, `requests = limits = 500m / 512Mi`, and `runtimeClassName: kata` from the gateway default. A stored template cannot name a runtime class (see gaps) |
| 4.3 | pass, through raw gRPC | profile `agentgateway-spike` (`POST /v1/chat/completions` on `agentgateway.agentgateway-system.svc.cluster.local:8080`, enforce, `stable_placeholder`) imported with the generated `ImportProviderProfiles` client; provider `agentgateway-key` created and updated through the SDK (responses carry no values); attached to running sandbox `p2` without a restart; the agent sees `OPENAI_API_KEY=openshell:resolve:env:s4534…` and the rendered `_provider_agentgateway_key` policy carries the rule |
| 4.4 | pass | `Config().Update` with step 3's policy as Go structs: v1 denies `whoami` (`000`) and PR `POST` (`403`); v2 adds `whoami`, first `200` after 9 s |
| 4.5 | pass | `Watch` delivered every phase change (Provisioning → Ready → Stopping → Stopped → Starting → Ready → Deleting) and policy version (v0 → v1 → v2); `GetLogs` returned 161 lines including the OCSF decisions |
| 4.6 | pass | `Workspaces().Create("spike-b", labels)`, `AddMember("spike-b", "spike-b-admin", Admin)`, `ListAllMembers` shows it |
| 4.7 | listed | gaps below |

## Gaps for the reconciler (4.7)

1. **SDK profile endpoints are lossy.** `types.NetworkEndpoint` in the SDK has only host, port and
   protocol; the gateway rejects an L7 endpoint without `access` or `rules` (`protocol requires
   rules or access to define allowed traffic`), and reading a profile back drops its rules. The
   wire type (`sandbox.v1.NetworkEndpoint`) is complete, so Koinon uses the generated
   `openshellv1.OpenShellClient` for profiles and the SDK for the rest, or files an upstream fix.
2. **Stored templates cannot set a runtime class.** `SandboxWorkloadTemplateSpec` has image, env,
   resources and driver config only; the runtime class comes from the gateway default or the
   per-sandbox `SandboxTemplate.RuntimeClassName`. Koinon sets it per sandbox (and every runtime
   class it uses needs the admission label from step 1).
3. **No workspace-wide watch.** `WatchSandbox` takes one sandbox name. The reconciler either holds a
   stream per sandbox, polls `List`, or watches the `agents.x-k8s.io` `Sandbox` objects in the
   cluster directly. Watch streams are chatty (repeated `MODIFIED` with an unchanged phase).
4. **Workspace-scoped profiles need `ProviderSpec.ProfileWorkspace`** on provider create; without it
   the gateway reports the profile as not found.
5. **No policy YAML parser in the SDK.** The reconciler renders `SandboxPolicy` structs directly,
   which is what it would do anyway.
6. **No SDK release tags**; Koinon pins a commit until upstream tags `sdk/go`.
