---
title: GitHub App
description: Create a GitHub App, install it and connect it to Koinon.
---

A Koinon connection to GitHub is one GitHub App installation. You create the App, install it on the
repositories your agents may use and give Koinon three things: the App ID, the installation ID and
a private key. Koinon's UI shows the same steps while you connect.

## 1. Create the App

Create the App under the account or organisation that owns the repositories. Give it a name and any
homepage URL. Turn **Webhook** off. Koinon does not use webhooks in v0.1.

Set these repository permissions and leave everything else at "No access":

| Permission | Access |
| --- | --- |
| Contents | Read and write |
| Issues | Read and write |
| Metadata | Read-only (GitHub sets it) |
| Pull requests | Read and write |

Do not grant Workflows. Koinon refuses changes under `.github/workflows/`, and the connection test
fails if the App can change workflows. Choose "Only on this account" for where the App can be
installed.

The agent never pushes. It reads the repository over HTTPS with a read-only token, and Koinon
creates the branch, the commit and the pull request after you approve. The write permissions are
for Koinon's calls.

## 2. Generate a private key

On the App's General page, under Private keys, choose Generate a private key. Keep the `.pem` file
for the next step, then delete it. Koinon stores the key encrypted in its database and never shows it
again.

## 3. Install the App

On the App's page choose Install App. Pick the account. Choose "Only select repositories" and select
the repositories your agents may work on. Each agent is narrowed further to the repositories in its
grant.

The installation ID is the number at the end of the installation's settings URL:
`https://github.com/settings/installations/<id>`, or
`https://github.com/organizations/<org>/settings/installations/<id>`.

## 4. Connect

In Koinon, open Connections and choose Connect GitHub. Enter the App ID, the installation ID and the
private key. Koinon checks the App, the installation and a short-lived token before it saves the
connection. Only org admins and owners can connect GitHub.

## GitHub Enterprise Server

Create the App on your server and connect it the same way. In the form's "GitHub Enterprise Server"
section, enter the server's base URL (`https://github.example.com`, only a host and an optional
port) and, when its TLS certificate is signed by a CA the system roots do not include, the CA bundle
(PEM certificates only). Koinon reaches the REST API at `/api/v3` on that host for the App flow,
installation tokens, commits through the Git Data API and pull requests, and trusts the bundle for
those calls and for git in sandboxes. The sandbox network policy names that host instead of
`github.com` and `api.github.com`. The host must pass the platform's egress URL policy: private and
internal addresses only when `egress.allowedInternalHosts` lists them. The chart's `github.apiUrl`
is only the API of connections that have no base URL.
