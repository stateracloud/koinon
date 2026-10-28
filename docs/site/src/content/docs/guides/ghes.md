---
title: GitHub Enterprise Server
description: Connect a GitHub App on your own GitHub Enterprise Server to Koinon.
---

A GitHub Enterprise Server (GHES) connection is a GitHub App installation like the one in
[GitHub App](/guides/github-app/), on your server instead of github.com. The permissions, the private
key and the installation ID work the same way, so create and install the App as that guide says, on
your server. Then connect it with the server's base URL.

## Connect

In Koinon, open Connections and choose Connect GitHub. Fill in the name, the App ID, the installation ID
and the private key, then open the section GitHub Enterprise Server and enter:

- the **Base URL** of your server, `https://github.example.com`: a host and an optional port, no path;
- the **CA bundle**, only when the server's TLS certificate is signed by a CA the system roots do not
  include (PEM certificates only).

Koinon checks the App, the installation and a short-lived token before it saves the connection, as for
github.com.

## What changes

- Koinon reaches the REST API at `/api/v3` on that host for the App flow, installation tokens, commits
  through the Git Data API and pull requests.
- The CA bundle is trusted for those calls and for git in sandboxes.
- The sandbox network policy names your host instead of `github.com` and `api.github.com`. An agent
  granted a repository on this connection can reach nothing else on GitHub.
- The host must pass the platform's egress URL policy: private and internal addresses only when
  `egress.allowedInternalHosts` lists them.
- Workflow files stay protected. Koinon refuses changes under `.github/workflows/` and under any extra CI
  directories you set on the connection.
- A scoped approval ([Scoped approvals](/guides/scoped-approvals/)) is tied to the connection and its
  base URL, so an approval given on github.com never covers the same `owner/repo` on your server.

The chart's `github.apiUrl` is only the API of connections that have no base URL. Leave it at
`https://api.github.com` when you use github.com next to GHES.
