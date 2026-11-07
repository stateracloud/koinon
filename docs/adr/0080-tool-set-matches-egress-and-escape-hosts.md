# 0080. The tool set matches the sandbox's egress, and escape cases observe proxy decisions

- Status: Accepted
- Date: 2026-10-29
- Relates to: [0025](0025-testing-strategy.md), [0045](0045-openshell-hardening-defaults.md), [0052](0052-opencode-v2-flavor.md), [0069](0069-native-model-formats.md), [0075](0075-isolation-tiers.md)

## Context

The rc.7 home-lab re-run found two gaps with one theme: what a test or an agent believes about
the network must be what the sandbox's proxy actually does.

1. Old issue 247. A research agent used OpenCode's built-in `webfetch` and `curl`. The sandbox policy
   blocked both, as designed, and the agent gave up instead of using the org's web-search MCP
   tools. A tool the network policy can only make fail should not be offered.
2. Old issue 244. Two escape cases used `.invalid` hosts. DNS failed inside the sandbox before the
   request reached OpenShell's proxy, so curl reported code `000` and neither case observed an
   allow or deny decision.

## Decisions

1. **One predicate says whether an agent has web egress:** `agentconfig.Config.WebEgress()`. It is
   false for every version today: a connector grant opens its connection's host only, the npm
   registry follows `harness.pluginInstalls`, and web research is an MCP tool that runs in the
   org's gateway, outside the sandbox. A future grant that opens arbitrary hosts adds its case
   there and nothing else changes.
2. **A flavor declares how to drop its built-in web tools:** `flavor.Descriptor.NoWebToolsArg`
   (OpenCode: `--no-web-tools`). koinon-agentd passes it when `WebEgress()` is false. The entry
   script adds a permission rule `{action: webfetch, resource: "*", effect: deny}` to
   `OPENCODE_CONFIG_CONTENT`; a denied tool is not offered to the model. Web search is already
   off for every agent (ADR 0052: `-opencode.websearch.*`, `-opencode.tool.websearch`). A flavor
   without built-in web tools leaves the field empty.
3. **The generated AGENTS.md says how to reach the web:** without web egress it gets a "Web
   access" section: no built-in fetch or search, `curl` and `git` reach only the hosts of the agent's repositories,
   web access goes through the organization's MCP tools, and if none is offered say so. With web
   egress the section is absent. The contract suite checks that the model is offered `webfetch`
   without the switch (the control) and neither it nor `websearch` with it.
4. **Escape cases must see a proxy decision.** curl code `000` is a test error (`answered` in
   `test/escape`), never an expected code, in every case that expects a policy answer. The
   direct-egress cases still expect `000`, because there the sandbox refuses the connection
   itself. The two cases from old issue 244 use hosts that resolve inside the sandbox:
   `KOINON_ESCAPE_GATEWAY_HOST` (no default: a non-loopback name the proxy resolves, with a server on
   4000; see the 2026-10-30 amendment: an allowed request gets the stand-in's 418 and a denied one
   gets 403; the case skips without it) and
   `KOINON_ESCAPE_GITEA_URL` (default `https://example.com/gitea`; every request is denied by
   policy before the proxy connects). OpenShell cases run only where `KOINON_OPENSHELL_ENDPOINT`
   is set (the home lab) and skip everywhere else, kind and CI included, so nothing about the kind
   runs changes.

## Amendment 2026-10-30: subagents are off (old issue 261)

The same rule governs OpenCode's `subagent` tool, for a different reason: a child session outlives a
cancelled turn and agentd records none of its events, so the wall clock (old issue 243) and the wrap-up
notice (old issue 183) could not stop it and traces (ADR 0074) would not show it. The config overlay denies
the `subagent` action for every agent, `explore` included, so the model is not offered the tool. The
contract suite checks the tool list. Allowing `explore` again needs agentd to advertise and record
child updates and cancel to cascade to children.

## Amendment 2026-10-30: the gateway stand-in is a real listener (old issue 270)

The rc.8 home-lab run showed that decision 4's stand-in does not work on OpenShell 0.1.5. With a
Service without endpoints, an allowed request whose upstream refuses the connection does not reach
the client as an HTTP 502: the sandbox sees a TCP failure (curl exit 7, code `000`). That is
indistinguishable from a request that never reached the proxy, which is what `answered` exists to
reject, so the allowed route could not be asserted.

The stand-in is now a server that answers: `escape-gateway-standin` in `koinon-fixtures`
(`test/fixtures/rt1/manifests/escape-gateway-standin.yaml`), `koinon-fakemodel -standin` on port 4000,
answering every request with an empty 418 and `X-Koinon-Standin: 1`. Its NetworkPolicy admits only
pods in OpenShell sandbox namespaces. The case expects 418 plus the marker for every model route
(the request crossed the policy and the far end answered), 403 for everything else on the gateway
(the policy denied it), and fails on anything else (`000`, 502, a 418 without the marker). The
home lab sets `KOINON_ESCAPE_GATEWAY_HOST=escape-gateway-standin.koinon-fixtures.svc.cluster.local`. The
kind and CI runs still skip OpenShell cases.

## Consequences

- An agent without web egress cannot waste a turn on `webfetch`; its instructions point to MCP.
- Adding a web-egress grant is one change in `WebEgress()`, with the hidden tool and the
  instructions following from it.
- A DNS miss in the escape suite fails loudly with the reason instead of passing or failing
  with a bare `000`.
- OpenShell always blocks loopback and link-local targets, so `localhost` cannot reach a stand-in
  and is not a default. The gateway case needs `KOINON_ESCAPE_GATEWAY_HOST` set explicitly. The 418
  with the real stand-in is still to be confirmed on the home lab.
