# Security policy

Koinon runs AI agents that act on real systems, so we take isolation and credential handling
seriously. A sandbox escape suite covers the isolation promises (ADR 0025). What runs where:

- CI runs the admission half on every pull request (`escape (admission)`): the admission policy
  that rejects unsafe sandbox specs.
- The OpenShell cases (no direct egress, no cloud metadata, no service account token, no secrets on
  disk) run on kind (`make test-escape-kind`) and on a maintainer's Kata cluster, not in CI.
- The Kata isolation tier is tested only on a maintainer's Kata cluster, because kind cannot run Kata.

## Reporting a vulnerability

Report vulnerabilities through GitHub
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository (**Security → Report a vulnerability**). Please do not open public issues for
security problems.

Include what you found, how to reproduce it, and the impact you expect. We aim to acknowledge
reports within 5 working days and to agree a disclosure date with you.

## Scope

In scope: the Koinon control plane, reconciler, agentd, Helm chart, flavor images and the policies
Koinon renders for OpenShell. Issues in upstream projects (OpenShell, Agent Sandbox, agentgateway,
harnesses) should go to those projects; tell us too if Koinon's defaults make them exploitable.

## Supported versions

Koinon is pre-alpha. Only the latest release receives fixes.
