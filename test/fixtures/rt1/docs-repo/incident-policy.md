# Incident policy

Owner: Platform. An incident is anything that harms customers or puts data at risk.

## Severity levels

| Level | Meaning | Examples | First response |
|---|---|---|---|
| 1 | Customer-facing outage or data loss | checkout down; database corruption | 15 minutes, 24/7 |
| 2 | Major degradation or exposed credentials with write access | errors over 5% for 10 minutes | 30 minutes, 24/7 |
| 3 | Minor degradation or contained exposure | one job failing; read-only key leaked | next working day |
| 4 | No customer effect | flaky test; cosmetic defect | normal backlog |

## Roles

- Incident commander (IC): runs the incident, decides, does not debug. Rotates with the on-call rota.
- Operations lead: does the technical work.
- Communications lead: updates the status page every 30 minutes for severity 1 and 2.
- Scribe: keeps the timeline in the incident channel.

## Process

1. Declare the incident in `#incidents` with a severity and a one-line summary.
2. Stabilise first: roll back or fail over before finding the root cause.
3. Customer data exposure must be reported to the Security lead within 1 hour of discovery.
4. Resolve when the symptom is gone and has stayed gone for 30 minutes.

## Review

- Severity 1 and 2 need a blameless review within 5 working days. Severity 3 needs one if it
  repeats.
- The review lists contributing factors, what went well, and actions with an owner and a date.
- Actions are tracked until done; the monthly Platform meeting checks overdue ones.

## Checks

- On-call handover notes list open incidents and open review actions.
- Quarterly: a game day exercises one severity 2 scenario.
