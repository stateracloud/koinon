# Security policy

Koinon runs AI agents that act on real systems, so we take isolation and credential handling
seriously. A sandbox escape suite covers the isolation promises (ADR 0025). What runs where:

- CI runs the admission half (`escape (admission)`): the admission policy that rejects unsafe
  sandbox specs. It is a required check that reports on every pull request, but the suite itself
  runs only when the change touches `charts/`, `test/escape/` or the packages the suite imports
  (the reconciler, agent config, execution, leak guard, test helpers and store), and always on a
  push to main (`hack/ci-changes.sh`).
- The OpenShell cases (no direct egress, no cloud metadata, no service account token, no secrets on
  disk) run on kind (`make test-escape-kind`) and on a maintainer's Kata cluster. No CI job runs
  them on every pull request yet (the leak guard escape case joins the PR kind e2e in #348).
- The Kata isolation tier is tested only on a maintainer's Kata cluster, because kind cannot run Kata.

## Reporting a vulnerability

Report vulnerabilities through GitHub
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository (**Security → Report a vulnerability**). Please do not open public issues for
security problems.

Include what you found, how to reproduce it, and the impact you expect. We aim to acknowledge
reports within 5 working days and to agree a disclosure date with you.

How we handle a report:

1. We confirm the issue and its severity in the private advisory, and keep the discussion there.
2. We prepare the fix in a private fork of the advisory and test it.
3. We request a CVE through GitHub Security Advisories (GHSA) when the issue affects a released
   version, and publish the advisory, the fix and the release together on the agreed date. The
   default is 90 days from the report, sooner when the fix is ready, later only by agreement.
4. If the issue is being exploited, we ship a fix or a mitigation first and publish the details
   after.

If private vulnerability reporting is unavailable, open a public issue that says only that you have
a security report and ask for a private channel; do not include details.

## Scope

In scope: the Koinon control plane, reconciler, agentd, Helm chart, flavor images and the policies
Koinon renders for OpenShell. Issues in upstream projects (OpenShell, Agent Sandbox, agentgateway,
harnesses) should go to those projects; tell us too if Koinon's defaults make them exploitable.

## Supported versions

Koinon is pre-alpha. Only the latest release receives fixes.
