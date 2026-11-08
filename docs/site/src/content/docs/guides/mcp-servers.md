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

## Web access goes through MCP servers

An agent's sandbox cannot reach the web: its network policy opens only the hosts of its grants. So the
harness's built-in web fetch is not offered to the agent, and its instructions say that web access goes
through the org's MCP tools. To let agents search or read pages, connect an MCP server that does it
(a search server such as one in front of SearXNG), and allow it for the agent.

## Add a server in the UI

Org admins and owners open **MCP servers** in the sidebar. Members do not see it.

- **Add server** takes a **name**, the **URL**, the **transport** (Streamable HTTP, or SSE for older
  servers) and, if the server needs one, a **credential**: a bearer token, or a custom header with a value.
  The credential is write-only. Koinon stores it encrypted and never shows it again. To change it, type a
  new one; to remove it, use **Remove credential**.
- The name is a short label of letters, digits and `-` (at most 24 characters). In the gateway the server
  is called `<org>-<name>`, and that is also the prefix of its tools in an agent's tool list, for example
  `acme-tracker_search`.
- If the URL is refused (a private, loopback or cluster-internal address), the form says why. Ask a
  platform admin to allow the host ([Upstream hosts](#upstream-hosts)).
- If the server asks each user to sign in, Koinon refuses it and says so. (Koinon makes one anonymous request
  to the new server to see; it carries no credential.)
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

Approved calls to a tier-Ask tool run through the gateway too, with an executor key for your org, so the
same egress rules apply and Koinon's API never holds the credential. A call is sent once: if the connection
drops after it was sent, the request fails with "outcome unknown" and is not retried; check the server.

Anyone who can create a labelled Secret in Koinon's namespace can define servers for any org: grant that
as you would grant cluster administration.

## Which tools an agent may call

Every tool a server lists starts at **Ask**. You decide, tool by tool, on the **MCP servers** page:

| Tier | What happens when an agent calls the tool |
| --- | --- |
| **Ask** (the default) | The call is recorded with its exact arguments as an approval card on the task, and the run pauses. After a human approves, Koinon calls the server itself and the run resumes with the result. |
| **Allow** | The agent calls the tool directly through the gateway. |
| **Deny** | No agent can call it. |

- A tool with the MCP `readOnlyHint` annotation shows "Suggested read-only". It is only the server's
  claim. Move it to Allow yourself if you agree.
- A tool the server adds later enters the ask tier for every agent that allows the whole server.
- If a tool's **description or input schema changes**, it shows a **Review** flag. A changed description
  is how a hostile server could try to steer an agent. An **Allow** tool that changes is **moved to Ask**
  until you review it; **Mark reviewed** clears the flag but keeps it in Ask, and you set Allow again if
  you trust the new version. Ask and Deny tools only get the flag. An approval request is tied to the
  version of the tool it was made against: if the tool changes before a human approves, Koinon will not
  run it.
- **Require two approvers** (per tool, off by default): the call needs two different admins or owners,
  and neither may be the person who created the task. The first approval leaves the card pending.
- Tool names must be at most what model APIs accept. Agents see `<org>-<server>-ask_<tool>` behind the
  harness' prefix, so a long server name leaves less room; a tool that does not fit, or has characters such
  as `.`, shows "unavailable" and is not offered.
- Tool names an agent sees always start with the gateway target: `acme-tracker_search`,
  `github_create_pull_request`. They do not change when an org adds a server.

Then allow servers per agent. On the agent's page, **MCP servers** lists the org's servers. Tick the ones
the agent may use, and, if you want, **limit to some tools**. Nothing is allowed by default. The change
takes effect within about a minute, without a new agent version.

The **FinOps analyst** template has no grants at all: it reads yesterday's costs every day at 07:00 UTC
(change the time and zone in the agent's triggers) through the cost, billing or metrics servers you allow
here, and replies with a three-part report. Allow those servers on the agent's page after you create it.

## What the text check does not catch

For tools that reach outside the platform, Koinon also compares the text of a call with the private
data the agent has read, and asks for approval on a match. This is a second alarm, not the main
control: the byte cap and the rule that a call after untrusted input needs approval do the real work.
It catches copies and simple encodings (base64, percent-encoding, escapes, hex), also when the text is
split across several arguments of one call.

Known limit: a secret split across two arguments and encoded as base64 with long runs of filler
between the characters (each half longer than 1024 bytes) is not matched. The same holds when that
padded base64 is itself wrapped in base64, `\u` escapes or percent-encoding. The same text in one
argument is matched. Do not rely on the text check against an agent that is trying to hide data; keep
tools that send data out in the Ask tier. The general fix is tracked in #369.

## Health

Koinon checks every server every 60 seconds through the gateway: it connects like an agent would and
lists the tools. A server you add, or whose URL or credential you change, is checked right away, as
soon as the gateway serves it. The result also keeps the tool list up to date.

- After **three failed checks in a row** the server is **Unhealthy**: the MCP servers page shows why
  (the gateway got no tools back: the server is unreachable, rejected the credential, or lists none), and its tools leave every
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
decisions and what agentgateway v1.7.0 does.
