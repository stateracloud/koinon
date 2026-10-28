# Access windows

Owner: Platform. Defines when production may be changed and by whom.

## Change windows

- Normal changes to production happen Monday to Thursday, 09:00 to 16:00 local time.
- No production changes on Fridays, weekends or public holidays, except fixes for severity 1 or 2
  incidents.
- A freeze runs from 15 December to 5 January. During a freeze only severity 1 fixes ship.

## Who may change what

| Role | Staging | Production |
|---|---|---|
| Developer | deploy and read logs | read-only dashboards and logs |
| On-call engineer | deploy and restart | restart, scale, roll back inside a window |
| Platform engineer | full | deploy inside a window, change infrastructure with a second reviewer |
| Automation (CI and agents) | deploy | deploy inside a window with a change ticket; no manual overrides |

## Break-glass

1. Request the break-glass role in the access tool and name the incident number.
2. Access lasts 2 hours and then expires; renew by repeating the request.
3. Every session is recorded. A reviewer from outside the on-call rota reads the log within
   2 working days.

## Automation rules

- Automation must use its own identity, never a person's credentials.
- Automation must not extend its own permissions or change its own access policy.
- An automated change outside the window is a policy violation even if it succeeds.

## Checks

- The deploy tool refuses production deploys outside the window unless an incident is attached.
- Monthly: Platform reviews all break-glass sessions and all out-of-window deploys.
