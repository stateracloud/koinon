# Step 3: policy

Run 2026-10-21 in sandbox `p2` (probe image, provider `gitea-bot`, policy
`hack/spike/policies/step3.yaml`, `ocsf_json_enabled=true`). The provider profile now also allows git
smart HTTP for `/koinon-spike/**`: `GET …/info/refs` and `POST …/git-upload-pack` only.

| # | Result | Evidence |
| --- | --- | --- |
| 3.1 | pass | `GET /api/v1/repos/koinon-spike/hello/contents/README.md` → `200`; `git clone` with `http.extraHeader` carrying the placeholder succeeds; `git-remote-http` is matched as a child of `/usr/bin/git` |
| 3.2 | pass | `POST /api/v1/repos/koinon-spike/hello/pulls` → `403 policy_denied`, logged `HTTP:POST [MED] DENIED … [reason:L7_REQUEST deny POST …]`; `git push` (`git-receive-pack`) also denied |
| 3.3 | pass | Ollama (`http://…:11434`), `https://example.com`, `http://example.com`: no connection (`curl` `000`); logged `NET:REFUSE … policy_dns_ineligible` and `NET:OPEN … DENIED … transparent_tcp_policy_denied` |
| 3.4 | pass | a copy of curl at `/tmp/mycurl` with the same request: `NET:OPEN [MED] DENIED /tmp/mycurl(0) -> git…:443` |
| 3.5 | pass | sandbox `p3` with no provider and no network policy: Gitea and `whoami` unreachable (`000`) |
| 3.6 | pass | `policy set` adding `whoami`: first `200` after 9 s, no restart |
| 3.7 | confirmed | `whoami` with `enforcement: audit` and the read-only preset: `POST` and `DELETE` → `200`. Shorthand log says `ALLOWED`; the JSON record keeps `action: Allowed` but `message: "L7_REQUEST audit POST … not permitted by policy"`. ADR 0045's forced `enforce` is needed |
| 3.8 | partial | OCSF JSON on supervisor stderr (`OCSF-JSON` lines): Network Activity (4001) has `actor.process.name` (binary), `dst_endpoint`, `action`, `firewall_rule`, `container.uid` (sandbox); HTTP Activity (4002) has method, URL, verdict, rule and sandbox but **no binary**. Enforcement held; the binary per HTTP decision is missing, so Koinon joins the connection event to its HTTP events. Partial, no stop (Andre, 2026-10-21) |

## Findings

1. **Deny by default holds at three layers**: DNS (non-policy names get no real answer),
   transparent TCP (connections to non-policy destinations refused) and L7 (method/path rules).
2. **Policy DNS answers every name with a synthetic `198.18.0.x` address** (`getent hosts
   example.com` → `198.18.0.4` in `p3`). Step 8 must check whether such lookups reach an upstream
   resolver, which would be a DNS exfiltration channel.
3. **Provider profiles compose with the sandbox policy.** The profile's endpoints become a
   `_provider_<name>` policy entry; the profile's read-only preset already denies the PR `POST`, so
   the explicit `deny_rules` in `step3.yaml` is defence in depth. Koinon can put write denials in
   the rendered sandbox policy without editing profiles.
4. **Landlock `hard_requirement` works** on this cluster (the sandbox started with it); ADR 0045
   should require it instead of the `best_effort` default.
5. **Audit mode is indistinguishable in the shorthand log**; only the JSON `message` says a rule
   was broken. Another reason to force `enforce`.
6. OCSF `actor.process.cmd_line` carries part of the agent's command line; logs can therefore hold
   whatever the agent puts in arguments (placeholders at most, never real values, per step 2).
