---
title: Run content retention
description: How long Koinon keeps what a run said and did, how an org admin changes it, and what stays after the content is removed.
---

Koinon stores the content of a run: the messages, thoughts and tool calls of its events. That
content is kept for **30 days after the run finished**, then removed. An org admin can change the
period for their org, from 1 to 365 days.

## What is removed, what stays

- Removed: once a finished run is older than the org's retention, the payload of its content events
  (messages, thoughts and tool calls) is blanked, and the text of prompt, permission and refusal
  events is stripped. The run is marked as purged, and the run page shows those events without their
  content.
- Stays: the run, its status and outcome, and every event row with its sequence number, kind and
  time, so the run page keeps its timeline. Events that are not content (such as a wrap-up notice
  with its spend) and the state fields of prompts, permission decisions and refusals (for example
  the decision and the count) keep their payload. Traces sent to a destination and the spend ledger are
  other records and are not touched.
- A run that has not finished is never purged.

## Change the period

In the org settings API, `GET` and `PUT /run-content-retention` of the admin API (`days`; leave it
out to return to the default), or `OrgSettingsService.SetRunContentRetention` for the console.
Admins and owners only. Every change is audited as `org.run_content_retention.set`.

Lowering the period removes older content at the next purge pass. Content that was removed does not
come back when the period is raised again.

## The purge job

`koinon-api` runs the purge once an hour, org by org, in batches of 1000 events, each batch in its
own short transaction. Running it again finds nothing left to do, and several replicas can run it at
once (they skip rows another batch holds).

## Old home backups

When an agent's home backup key changes, its backups start again in a new repository and the old
one is kept for **7 days** by default, then deleted, but only once the new repository holds a
checkpoint (or at the deadline even without one, if the old one's password was lost). An org admin can change that period,
from 1 to 90 days: `GET` and `PUT /home-backup-retention` of the admin API (`days`; leave it out or
send 0 to return to the default), `OrgSettingsService.SetHomeBackupRetention`, or **Old home
backups** on the console's observability settings page. Admins and owners only; every change is
audited as `org.home_backup_retention.set`. The period applies to keys changed after the change; a
repository already retired keeps its deadline.
