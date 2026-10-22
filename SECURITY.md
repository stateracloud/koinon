# Security policy

Koinon runs AI agents that act on real systems, so we take isolation and credential handling
seriously. A sandbox escape suite will run against every pull request (ADR 0025).

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
