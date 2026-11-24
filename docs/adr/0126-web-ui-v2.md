---
status: accepted
date: 2026-11-24
area: frontend-ux
tags: [ui, multi-tenancy, pricing, mcp]
supersedes: ["0018", "0057"]
relates: ["0020", "0022", "0056", "0065", "0067", "0125"]
---
# 0126. Web UI v2: brand look, org home, platform console and new-agent flow

- Status: Accepted (maintainer decisions of 2026-11-24; mockups approved: "Mocks lgtm")
- Date: 2026-11-24
- Area: frontend-ux
- Tags: ui, multi-tenancy, pricing, mcp
- Supersedes in part: [0018](0018-v1-screens.md) (the screen list, the home screen, the platform admin screen and the
  personal org in the org list), [0057](0057-mcp-servers.md) (where the MCP server screens live: the org page and the
  platform list)
- Relates to: [0020](0020-agent-builder-and-templates.md), [0022](0022-frontend-architecture.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0065](0065-work-model-tasks-inbox.md),
  [0067](0067-concierge.md), [0125](0125-hide-unreviewed-mcp-tools.md)
- Mockups: [Koinon UI v2 mockups](https://claude.ai/artifact/5AEd7tMCnqrnUkkWmD9Z4E) (canvas; the board names below
  are the artboard names on that canvas)

## Context

The UI overhaul starts now, beside the Substrate work (maintainer, 2026-11-24). The web UI of v0.1 has these
problems (facts from `origin/main` on 2026-11-24):

- The look is the stock shadcn "new-york, neutral" theme. Every colour is grey except `--destructive`. The font is
  `system-ui`. There is no logo file (`web/src/index.css:2`, `:94`).
- There is no success, warning or info colour. Code uses raw Tailwind `amber-*`, `emerald-*` and `green-*` classes
  in 10 places.
- The `dark:` variant never applies. `@custom-variant dark (&:is(.dark *))` (`web/src/index.css:6`) needs a `.dark`
  class, and no code sets it. Dark mode works only through the media query on the tokens.
- The inbox count badge is white text on `bg-amber-600` at `text-xs` (`web/src/pages/app-layout.tsx:96`). Its
  contrast is about 3:1, under the 4.5:1 that WCAG 2.2 AA requires for text.
- The home page is the inbox. Agents are a flat list. There is no team and no "reports to".
- A platform admin cannot open an org that they are not a member of: `OrgProvider` reads only `WhoAmI.memberships`
  (`web/src/lib/org.tsx:21`). The "acting as platform admin" note (`app-layout.tsx:177`) never shows. The platform
  pages are three links in the org sidebar.
- `PlatformService` is empty (`api/proto/koinon/platform/v1alpha1/platform.proto:11`). There is no API to list,
  create or change orgs.
- Only `CreateAgentFromTemplate` exists (`api/proto/koinon/agent/v1alpha1/agent.proto:17`). There is no
  `UpdateAgent` and no agent without a template. Templates have no org (`internal/db/migrations/00002_agents.sql:10`).
- Prices have three layers: the org price, the platform price ([0098](0098-price-catalogue-updates-from-public-sources.md))
  and the shipped default ([0056](0056-model-providers-pricing-and-budgets-per-org.md)). The UI does not show which
  layer prices a model.
- Every new user gets a personal org at first login (`internal/auth/bootstrap.go:346-374`, ADR 0018). So the
  "no org" screen never shows.

ADR 0018 fixed nine screens and the automatic personal org in October 2026. The first UI review (2026-10-28), the
brand canvas (2026-10-28) and the questions of 2026-11-24 changed parts of it. This ADR records the result. The
research is in [ui-foundations.md](../research/ui-foundations.md).

## Decision

### 1. Look: brand 1E Pastel, dark by default

1. **Brand.** The brand is direction 1E "IDE-inspired", palette Pastel. The dark theme is the default. Grounds are
   flat: no texture, noise or gradient in the product UI.
2. **Fonts.** Geist for all text and Geist Mono for code-like text (tool names, branches, paths, hashes, run IDs,
   diffs, JSON). Both are SIL OFL 1.1. Koinon serves the font files itself; the UI must not load fonts from a CDN at
   run time _(inferred: an install can be offline)_. Weights are 400 (text), 500 (labels, buttons) and 600
   (headings). Numbers use tabular figures.
3. **Logo.** The K monogram with the gold lower leg (viewBox `0 0 48 48`: stem `rect x=6 y=6 w=11.5 h=36 rx=3`, upper
   arm `M19.5 24L31.5 6H43.5L31.5 24Z`, lower leg `M19.5 24H31.5L43.5 42H31.5Z`). The stem and the arm use the brand
   colour, the leg uses the logo gold. The wordmark is "Koinon" in Geist 600 at -0.045em letter spacing. The favicon
   is the dark mark on a `#262538` tile.
4. **No pink.** The palette has no pink or magenta.
5. **Colour has a meaning.** Colour shows a status or an identity. Purple is the action colour (buttons, links,
   focus). Amber means "waiting on you".
6. **Contrast.** Every text colour on every ground it uses has a contrast of at least 4.5:1 (WCAG 2.2 AA). Large text
   and non-text parts (borders, icons, focus rings) have at least 3:1. A test checks the token pairs.
7. **Light theme is a draft.** The maintainer said the light theme "still needs refinement". The L2 "tinted daylight"
   values below are the draft. The UI may ship them behind the theme switch, but they are not final.
8. **Theme switch.** The user can pick System, Dark or Light. The UI sets `data-theme` on `<html>`. "System" follows
   `prefers-color-scheme`; with no preference, the UI uses dark. One dark-mode strategy replaces the dead `.dark`
   variant: the `dark:` variant follows `data-theme`.

### 2. Theme tokens

Role tokens. Components use only these names, never a raw Tailwind palette colour. The contrast column is the text
colour on the page ground (dark / light).

| Token | Dark (default) | Light L2 (draft) | Note |
| --- | --- | --- | --- |
| `background` (page, sidebar) | `#1C1B29` | `#DDDDF3` | |
| `card` | `#302F41` | `#F5F6FF` | |
| `raised` | `#414053` | `#FDFDFF` | |
| `overlay` (menus, dialogs) | `#555468` | `#FFFFFF` | |
| `foreground` | `#EAEEFE` | `#24233C` | 14.7:1 / 11.4:1 |
| `muted-foreground` | `#ACB1CB` | `#555571` | 8.0:1 / 5.4:1 |
| `border` (controls) | `#9090A4` | `#727286` | non-text, at least 3:1 |
| `divider` | `#616174` | `#BBBBD1` | decorative |
| `primary` (action) | `#CBA6F5` | `#6E3EAD` | 8.3:1 / 5.3:1 |
| `primary-foreground` | `#1C1B29` | `#FFFFFF` | 8.3:1 / 7.1:1 on `primary` |
| `link`, `ring` | `#D2B3F7` | `#65379F` | 9.3:1 / 6.0:1 |
| `attention` (fill) | `#F7C68C` | `#E3993E` | dark text on it: 10.8:1 / 6.4:1 |
| `warning` (text) | `#F7C68C` | `#8A5516` | 10.8:1 / 4.6:1 |
| `success` | `#A7DCA1` | `#306F35` | 10.8:1 / 4.6:1 |
| `danger` | `#FA9494` | `#B13530` | 7.9:1 / 4.6:1 |
| `destructive` (fill) | `#F38E8E` | `#B33731` | |
| `info` | `#90B7F8` | `#2763A8` | 8.3:1 / 4.6:1 |
| `idion` (personal space, later) | `#8CD6CB` | `#067067` | |
| `logo-gold` (K leg only) | `#F7C68C` | `#9A6200` | |

Categorical colours for agents and teams (avatars, team dots), dark / light: orange `#F6AE84` / `#B1512D`, gold
`#D6C665` / `#8C6801`, green `#A7DCA1` / `#357339`, teal `#8CD6CB` / `#067067`, sky `#48A5C7` / `#3299B4`, blue
`#90B7F8` / `#2964AA`, lavender `#C4C4FD` / `#483C95`, slate `#788597` / `#313E4F`. Text on a categorical fill is
the dark `background` colour.

### 3. Task-state colours

| State | Colour | Dark badge (text on fill) | Contrast |
| --- | --- | --- | --- |
| Submitted | blue | `#90B7F8` on `#2B3850` | 5.8:1 |
| Working | purple | `#D2B3F7` on `#3E3745` | 6.3:1 |
| Blocked on you | amber | `#1C1B29` on `#F7C68C` | 10.8:1 |
| Completed | green | `#A7DCA1` on `#343E33` | 7.1:1 |
| Failed | red | `#FA9494` on `#483535` | 5.3:1 |
| Canceled | grey, hollow | `#ACB1CB`, 1 px `#9090A4` border | 6.2:1 on `card` |

- A state badge always has the state name as text. Colour is never the only signal.
- Working uses purple, the action colour. A badge is a tinted fill with a dot; an action is a solid button. The two
  must not look the same.
- The inbox count badge uses the `attention` fill with the dark `background` text (10.8:1). This replaces the 3:1
  badge.
- The light values of the state badges come from the same roles (`info`, `primary`, `attention`, `success`,
  `danger`, `muted-foreground`). They are part of the light draft.

### 4. Information architecture

**Org area** (one org at a time; [board "Org home: agent cards"](https://claude.ai/artifact/5AEd7tMCnqrnUkkWmD9Z4E)):

- The sidebar, from top to bottom: the logo; the org switcher; Overview (`/`); Inbox (`/inbox`, with the count
  badge); Connections; a "Settings" group with Org settings, Providers, MCP servers (`/mcp-servers`), Prices,
  Budgets, Observability and API tokens; at the bottom, the "Platform console" link (platform admins only) and the
  user.
- Items that a role cannot use do not show.
- Old URLs redirect to the new ones (`/` inbox to `/inbox`, `/servers` to `/mcp-servers`, `/board` to the Board
  tab).

**Org home (Q1).** The overview has a "Waiting on you" strip (at most 5 items and an "Open inbox" link), then three
tabs:

- **Agents** (default): agent cards grouped by team. A card shows the name, the model, the state (Active, Draft,
  Unhealthy), "reports to", today's spend against the daily limit, running now or the last run, the next scheduled
  run and the count of items waiting on you. A draft card has "Review and activate".
- **Chart**: the same agents as a reports-to chart, one tree per team. Only agents are boxes. A person shows as a
  "Reports to <name>" label, not as a box. The chart has a keyboard and screen-reader equivalent (a tree).
- **Board**: the task board ([0065](0065-work-model-tasks-inbox.md)).

Reason: the research shows that box charts fail above about 30 nodes, on phones and with screen readers
(`ui-foundations.md:111`). Cards by team scale to the v0.2 scale track (1k to 10k agents). The chart keeps the
reports-to data model of the first review. Each agent gets two new fields: `team` (a label) and `reports_to` (an
agent or an org member).

**Org switcher (Q3, Q4).** The switcher is a menu. It lists the team orgs of the user with the role. It does not
list the personal org until v0.2. For a platform admin it has a "Platform console" item. The personal org stays in
the data model ([0018](0018-v1-screens.md), [0019](0019-personal-agents-and-teams.md)).

**First login (Q3).** A user with no team org sees a first-login screen, not an empty app. The screen shows the
ways in that exist for that user ([board "First login"](https://claude.ai/artifact/5AEd7tMCnqrnUkkWmD9Z4E)). The
ways that need a new API (invites, create org) are open items (section 7).

**Platform console (Q4).** The console is its own area at `/platform` in the same single-page app, with its own
sidebar: Home, Orgs, Usage and cost, Providers, Templates, MCP servers, Platform settings, and "Back to <last org>".
A label "Platform console" sits under the logo.

- A platform admin lands in the last org that they used after login, not in the console. The console opens from
  the org switcher or from the sidebar link.
- From the Orgs table, a platform admin can open any org. When the admin is not a member of that org, the org area
  shows an "acting as platform admin" banner on every page, with a link back to the console. The banner stays until
  the admin goes back to the console.
- Reason: org scope and platform scope stay apart, and the org sidebar stays short. ADR 0018 rejected a separate
  admin application; this is not one. It is one more area of the same app with role-gated routes.

### 5. New agent (Q5)

- "New agent" opens a card gallery: "Blank agent", the platform templates and the org templates, in groups.
- A template card opens a short wizard: Harness, Access, Review. The review step lists every choice with a "Change"
  link.
- "Blank agent" opens one form in sections (name and instructions, model and harness, access, triggers, limits).
- Both paths have "Save as template". It makes an org template.
- Every new agent is a draft. It does not run, and its triggers are off, until an org admin or owner activates it
  ([0020](0020-agent-builder-and-templates.md)). The agent page of a draft shows a notice with "Activate" for an
  admin. A member who is not an admin sees "Waiting for an admin to activate" and no button.
- The draft page has no "Send back to <person>" button. That action does not exist in the product.
- Reason: the template path keeps the review step of the 2026-10-23 decision 10 and the ADR 0020 activation. The blank
  path gives experts one form.

### 6. Prices, concierge and MCP pages

- **Prices (Q6).** Each price row has one of three badges: "Org override", "Platform" or "Default". A one-line legend
  above the table says what each one means. An org override row shows the price that it replaces, struck through.
  Reason: the three layers are real, and a platform price change must be visible to an org admin.
- **Concierge (Q7).** The concierge ([0067](0067-concierge.md), v0.3) gets a designed slot: a global side panel that
  opens from any page ([board "Concierge slot"](https://claude.ai/artifact/5AEd7tMCnqrnUkkWmD9Z4E)). The UI renders
  nothing for it until v0.3. There is no disabled control and no placeholder text.
- **MCP servers.** The org page moves from `/servers` to `/mcp-servers` under Settings. The platform console has an
  MCP servers item. The review of new tools follows [0125](0125-hide-unreviewed-mcp-tools.md): the org page shows the
  tools that wait for an admin review.

### 7. Open items

The mockups show these items. They need backend work that no decision covers yet. The UI must not build them until a
decision exists, and it must not invent their behaviour.

| Item | Board | State |
| --- | --- | --- |
| "Create org" in the switcher, the console and the first-login screen | Org switcher open, First login, Platform console: orgs | Needs a decision (who can create, self-service setting) |
| Org states "Archived" and "Near budget" in the Orgs table | Platform console: orgs | Needs a decision (org archive and delete, Q8 of the prep; "near budget" threshold) |
| Invites and IdP-mapped orgs on the first-login screen | First login | Needs a decision (membership API) |
| Failure reason "run cap reached" on a failed task | Inbox | Needs a decision (how a run-cap stop reaches the task) |
| The MCP tools-review notice on the console home (all orgs) | Platform console: home | The org-level review is decided ([0125](0125-hide-unreviewed-mcp-tools.md), issue #1216). The cross-org list for platform admins needs a decision |
| Edit and Remove of an org price override in the table | Model prices | Needs a decision (edit in place against a new price version; `SetPrice` and `RetirePrice` exist) |
| The light theme (L2) | Light theme (draft) | Draft; the maintainer asked for refinement |
| A high-contrast theme | none | Proposed in the design system only |
| Task ageing colours (time waiting on you) | none | Open: what the age counts from |
| Trademark search on the K monogram | none | Must happen before Koinon is public |

## Alternatives considered

- **Org chart as the default view.** Rejected: poor above about 30 agents, needs pan and zoom, and needs a separate
  accessible tree anyway.
- **Chart for small orgs, list above a threshold.** Rejected: the view changes under the user as the org grows.
- **Keep the neutral greys and add only status colours.** Rejected: the brand canvas of 2026-10-28 settled 1E Pastel,
  and the maintainer liked the dark theme.
- **One sidebar with a bigger "Platform" group.** Rejected: about 8 more items, and it mixes org scope and platform
  scope.
- **The console as the landing page for platform admins.** Rejected: most platform admins also work in an org.
- **One form for every new agent.** Rejected: a long form for the common template case, and no review step.
- **Two price badges ("Default", "Override").** Rejected: it hides a platform price change from org admins.
- **A disabled concierge input before v0.3.** Rejected: a dead control on the home page.
- **A "Personal" sidebar entry now.** Rejected: a screen with no content until the personal agent (issue #11).

## Consequences

- Positive: one token set replaces the raw palette classes and the dead dark variant. Later pages restyle against
  it.
- Positive: platform admins can see and open every org. Org admins see where each price comes from.
- Positive: the home page answers "what do my agents do, and what waits on me" on one screen.
- Negative: the overview, the chart, the blank agent and the console need new API: agent summaries with paging,
  `team` and `reports_to`, a members read, an org list for platform admins, `UpdateAgent`, an agent without a template,
  org templates, an org-level price explain and a usage read model. Each lands before its page.
- Negative: two sidebars (org and console) to keep in step.
- Negative: Geist adds two font files to the web bundle _(inferred)_.
- Negative: the light theme ships as a draft and can change. Screenshots and tests that pin light colours will need
  updates _(inferred)_.
- Follow-ups: the UI v2 issue list (foundation first, then API, then pages). The open items of section 7 need their own
  decisions.

## Open questions

- Section 7 lists them. Each one gets an issue only when a decision covers it.
- Is a person in "reports to" any org member, or only admins and owners? (Q1 follow-up; the members read API decides
  the default.)

## Upstream verification

- Geist and Geist Mono: SIL Open Font License 1.1 (vercel/geist-font). Checked by the brand work on 2026-10-28; not
  checked again for this ADR.
- No other upstream dependency.

## Links

- Mockups canvas: https://claude.ai/artifact/5AEd7tMCnqrnUkkWmD9Z4E (boards: Brief, Org home: agent cards, Org home:
  Chart tab, Org switcher open, First login, New agent: gallery, Template wizard: review step, Blank agent: one form,
  Agent detail: draft, Inbox: task state colours, Model prices: three badges, Platform console: home, Platform
  console: orgs, Org opened from the console, Concierge slot, Light theme draft)
- Brand canvas: https://claude.ai/artifact/D5aJ5ZjJRrTfesJ2qgMCNj; design system "Koinon":
  https://claude.ai/artifact/EEH4PfM1LMw5eNTHNWWaHM
- [ui-foundations.md](../research/ui-foundations.md)
- ADR [0018](0018-v1-screens.md), [0019](0019-personal-agents-and-teams.md), [0020](0020-agent-builder-and-templates.md),
  [0022](0022-frontend-architecture.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md),
  [0057](0057-mcp-servers.md), [0065](0065-work-model-tasks-inbox.md), [0067](0067-concierge.md),
  [0098](0098-price-catalogue-updates-from-public-sources.md), [0125](0125-hide-unreviewed-mcp-tools.md)
