# Northwind Platform Handbook

Fictional. Northwind Logistics does not exist; this handbook is test content for Koinon's RT1
story test. Policies here are written to be plausible, not to be good advice.

Owner: Platform team. Last full review: 2026-09-01. Questions: #platform in chat.

## Index

| Page | What it covers | Owner |
|---|---|---|
| [Secret handling](secret-handling.md) | Where credentials live, who may read them, rotation | Security |
| [Access windows](access-windows.md) | When and how people and automation may change production | Platform |
| [Backup and restore](backup-and-restore.md) | What is backed up, how often, how restores are proven | Platform |
| [Dependency upgrades](dependency-upgrades.md) | How often and how we upgrade libraries, images and charts | Platform |
| [Incident policy](incident-policy.md) | Severity levels, roles, timelines, reviews | Platform |
| [Glossary](glossary.md) | Terms used across these pages | Platform |

## How to read these pages

- "Must" is a rule. "Should" is the default; deviate and write down why.
- Every rule has a check: a command, a dashboard or a review step. Ask for it if a page lacks one.
- Pages are short on purpose. Anything that needs more than 60 lines belongs in a runbook.
