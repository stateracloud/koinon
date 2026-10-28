---
title: Gitea
description: Create a bot on your Gitea server, give it two tokens and connect it to Koinon.
---

A Koinon connection to Gitea is one bot account with two access tokens. Gitea cannot mint tokens for
one use or limit a token to some repositories, so Koinon compensates in three ways: the bot is a
collaborator on exactly the repositories agents may work on, the sandbox's network rules name only the
repositories of the agent's grant, and the token that can write never leaves Koinon's API.
[ADR 0063](https://github.com/stateracloud/koinon/blob/main/docs/adr/0063-forge-credential-classes.md)
has the reasoning. Koinon's UI shows the same steps while you connect.

You need a Gitea server that Koinon and the agents' sandboxes can reach (the connector reads the `/api/v1` REST
API; ADR 0062 was checked against Gitea 28.2), and admin rights on the repositories the agents will use.

## 1. Create a bot

Create a user on your Gitea server just for Koinon, for example `koinon-bot`. If your Gitea has bot
accounts (version 28 and later), create it as one. Give it an address you control and a long random
password, and never sign in as it except to create the tokens. One bot belongs to one connection.

## 2. Add the bot to your repositories

On each repository agents may work on, open Settings, Collaborators and add the bot with Write
permission. Do not make it an owner, an administrator or a member of an organisation team that sees
more than you intend. A Gitea token is not narrowed per repository, so what the bot can see is what a
token can reach. Each agent is narrowed further to the repositories in its grant: its sandbox can
reach only those.

## 3. Create the two tokens

Sign in as the bot (or create the tokens for it as an administrator), open Settings, Applications and
create two tokens:

| Token | Scopes | Who uses it |
| --- | --- | --- |
| Read token | `read:repository`, `read:issue`, `read:user` | Sandboxes, as an OpenShell placeholder. The real value never enters a sandbox, and it is sent only to your Gitea server. |
| Write token | `write:repository`, `write:issue`, `read:user` | Koinon's API only, after you approve a change: it pushes a new branch, opens the pull request and posts comments. |

Gitea tokens do not expire. Koinon shows how old each token is and warns after 90 days. It never
rotates a token and never refuses to work because of its age: to replace one, create a new token and
enter it again.

## 4. Connect

In Koinon, open Connections and choose Connect Gitea. Only org admins and owners can connect Gitea.
Enter:

- a name, which shows when you grant the connection to an agent;
- the server URL, `https://host[:port]` with a path if Gitea is served under one (for example
  `https://git.example.com/gitea`). Koinon reaches the REST API at `/api/v1` under it;
- the read token and the write token;
- optionally, a private CA bundle (PEM certificates only), when the server's certificate is signed by a
  CA the system roots do not include, and extra CI directories (below).

Koinon runs a check that writes nothing to Gitea and shows each step's result:

1. the server answers, and its version;
2. both tokens authenticate as the same bot;
3. the repositories the bot can see, the permission on each and how many it can push to. Review the list:
   it should be exactly the repositories you added. The check fails only when the bot sees none or can
   push to none;
4. the read token cannot write: Koinon sends a write that Gitea must refuse before it reads the request,
   so nothing is created. If Gitea accepts it, the token has a write scope and must be replaced.

The host must pass the platform's egress URL policy: a private or internal address works only when
`egress.allowedInternalHosts` lists it ([Models](/guides/models/) explains the same rule for model
providers).

A GitOps install can declare the connection with a Secret instead of the form; the keys are `baseUrl`,
`readToken` and `writeToken`, plus the optional CA bundle and CI directories.

## What agents do on Gitea

The agent builder offers the template Repo helper (Gitea). It is the repo-helper agent on a Gitea
grant: the agent clones the repository read-only, makes the change in its sandbox and asks Koinon to
open a pull request. You approve it, and Koinon pushes the branch from its API with the write token
and opens the pull request. The agent builder and the connection page warn that this credential is
weaker than a GitHub App's. All presets (Read only, Standard, Trusted) stay available.

Two more templates work on Gitea. **Researcher** has a read-only grant on one to five repositories
(`repo.read` only, so it cannot push or propose anything) and answers a question over several runs with
a cited Markdown report; it keeps its notes in its session directory (`/sandbox/work/notes`). **LCM upgrader** checks one repository every
Monday at 07:00 UTC for outdated Helm charts, images, Argo CD apps and dependencies, then asks "Create
the PR?" with up to three low-risk upgrades and opens a pull request only after a yes (you approve that
pull request too). Both read release notes and the web only through MCP servers you allow for the agent
([MCP servers](/guides/mcp-servers/)). A scheduled task has no creator, so the trigger's instruction is a form: on Edit triggers
([Triggers](/guides/triggers/)) set the `Repository:` line (LCM upgrader) and replace `Ask: none` with
`Ask: name@example.com` (or a user ID), the person whose inbox gets the question. With `none` the agent lists its
proposal in its reply instead.

## Workflow files

A branch pushed to Gitea can start Gitea Actions, and nothing on the credential can refuse that.
Koinon therefore refuses a pull request that changes `.gitea/workflows/`, `.gitea/scoped_workflows/` or
`.github/workflows/`, plus any directories you list as extra CI directories on the connection (use
them when your server sets `WORKFLOW_DIRS`). The approval card warns about the same paths.
