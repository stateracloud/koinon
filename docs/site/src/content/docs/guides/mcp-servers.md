---
title: MCP servers
description: Connect remote MCP servers to an org, control which tools agents may call, and see when a server is down.
---

An MCP server gives agents tools: search a wiki, read a tracker, file an issue. In v0.1 you connect
**remote** servers: servers that run elsewhere and that you reach by URL. Koinon does not run servers
for you yet, and a server that wants each user to sign in with OAuth is not supported yet
([What is not in v0.1](#what-is-not-in-v01)).

Agents never see a server's credential. They talk to agentgateway; agentgateway adds the credential
to the request it sends to the server. Each org has its own servers, and an agent only ever sees the
tools of servers of its own org that it was allowed.

## Add a server in the UI

Org admins and owners open **Servers** in the sidebar. Members do not see it.

- **Add server** takes a **name**, the **URL**, the **transport** (Streamable HTTP, or SSE for older
  servers) and, if the server needs one, a **credential**: a bearer token, or a custom header with a value.
  The credential is write-only. Koinon stores it encrypted and never shows it again. To change it, type a
  new one; to remove it, use **Remove credential**.
- The name is a short label of letters, digits and `-` (at most 24 characters). In the gateway the server
  is called `<org>-<name>`, and that is also the prefix of its tools in an agent's tool list, for example
  `acme-tracker_search`.
- If the URL is refused (a private, loopback or cluster-internal address), the form says why. Ask a
  platform admin to allow the host ([Upstream hosts](#upstream-hosts)).
- If the server asks each user to sign in, Koinon refuses it and says so.
- A new credential takes about three minutes to reach the gateway. Until then the server is not served.

The same operations are in the admin API: `GET`, `POST` `/admin/v1/mcp-servers`, `PATCH` and `DELETE`
`/admin/v1/mcp-servers/{id}`, and the tools below (see `/admin/v1/openapi.yaml`).

## Declare a server with a Secret

Like model providers, a server can be declared by a labelled Kubernetes Secret in Koinon's namespace
(Argo CD's
[declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/) style):

```yaml mcp-server
apiVersion: v1
kind: Secret
metadata:
  name: tracker
  namespace: koinon
  labels:
    koinon.dev/secret-type: mcp-server
    koinon.dev/org: default
stringData:
  name: tracker
  url: https://mcp.tracker.example.com/mcp
  transport: streamable-http
  credentialHeader: Authorization
  credentialScheme: Bearer
  credential: replace-me
```

- `credentialHeader` defaults to `Authorization`. `credentialScheme: Bearer` sends `Bearer <credential>`;
  leave it out and the credential is the whole header value (`X-Api-Key: <credential>`). Leave all three
  out for a server without a credential.
- A declared server is read-only in the UI and the admin API, and shows "Managed by Secret tracker". If a
  server with the same name was added in the UI, the declared one wins and the UI one is disabled, with
  the reason.
- Delete the Secret and the server is disabled, not deleted, so its tool decisions and the audit trail
  stay; restore the Secret and it comes back. An admin may delete a removed server for good.
- Setting a tool's tier on a declared server still works in the UI: that is a decision about risk, not
  something a Secret declares.
- A Secret that does not parse is ignored with a Warning Event on the Secret, which never quotes a value.

Anyone who can create a labelled Secret in Koinon's namespace can define servers for any org: grant that
as you would grant cluster administration.

## Which tools an agent may call

Every tool a server lists enters the **ask** tier. You decide, tool by tool, on the **Servers** page:

| Tier | What happens when an agent calls the tool |
| --- | --- |
| **Ask** (the default) | The call is recorded with its exact arguments as an approval card on the ticket, and the run pauses. After a human approves, Koinon calls the server itself and the run resumes with the result. |
| **Auto** | The agent calls the tool directly through the gateway. |
| **Never** | No agent can call it. |

- A tool with the MCP `readOnlyHint` annotation shows "Suggested read-only". It is only the server's
  claim. Move it to Auto yourself if you agree.
- A tool the server adds later enters the ask tier for every agent that allows the whole server.
- If a tool's **description or input schema changes**, it keeps its tier and shows a **Review** flag. A
  changed description is how a hostile server could try to steer an agent, so read it, then move the tool
  to another tier or choose **Mark reviewed**. The flag never blocks the tool.

Then allow servers per agent. On the agent's page, **MCP servers** lists the org's servers. Tick the ones
the agent may use, and, if you want, **limit to some tools**. Nothing is allowed by default. The change
takes effect within about a minute, without a new agent version.

## Health

Koinon checks every server every 60 seconds through the gateway: it connects like an agent would and
lists the tools. The result also keeps the tool list up to date.

- After **three failed checks in a row** the server is **Unhealthy**: the Servers page shows why
  (connection refused, the server rejected the credential, no tools came back), and its tools leave every
  agent's tool list. Other servers are not affected.
- At the start of a session, Koinon tells an agent which of its allowed servers are down.
- One successful check makes the server healthy again, and the tools return.

## Upstream hosts

A server URL may not point at loopback, link-local, private or cloud metadata addresses, at a name
without a dot, or at a Service in Koinon's own namespace. This is the policy for model providers too. A
platform admin allows an internal server in the chart value `egress.allowedInternalHosts` (the old name
`providers.allowedInternalHosts` still works for one more release) and, with the chart's gateway egress
policy on, in `networkPolicy.gatewayEgressTo`. Public servers need neither.

## What is not in v0.1

- Servers that run inside the cluster, started by Koinon, and stdio servers.
- A catalog of servers from the MCP Registry.
- Servers that need per-user OAuth sign-in. Koinon detects them when you add one (a 401 with an OAuth
  resource metadata URL) and refuses it with a message. v0.2.

See [ADR 0057](https://github.com/stateracloud/koinon/blob/main/docs/adr/0057-mcp-servers.md) for the
decisions and what agentgateway v1.6.0 does.
