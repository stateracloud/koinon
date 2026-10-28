---
title: Koinon documentation
description: Install Koinon, connect GitHub and run your first agent.
---

Koinon runs AI agents in sandboxes on your Kubernetes cluster. You give an agent a task. The
agent works in its sandbox and asks you before it changes anything outside it.

Koinon is pre-alpha. These pages describe what `main` does today. Where a v0.1 change is decided but
not built yet, the page says so and links the decision.

## Start here

1. [Prerequisites](/prerequisites/). What the cluster needs, with pinned versions.
2. [Quickstart](/quickstart/). From `helm install` to an approved pull request in about 15 minutes.

## Guides

- [Models](/guides/models/). Providers and models per org: Secrets, the admin API, OpenRouter with the routing guard, Ollama.
- [GitHub App](/guides/github-app/). Create the App and connect it.
- [OIDC login](/guides/oidc/). Sign in with your identity provider.
- [Gateway modes](/guides/gateway-modes/). Standalone, Kubernetes and shared agentgateway.

## Keeping these pages true

Every shell command in the quickstart is run by the quickstart end-to-end test on a kind cluster.
The browser steps are run by a Playwright spec. If a command here changes, the test changes with it.
[How the check works](/quickstart/#how-these-steps-are-tested).
