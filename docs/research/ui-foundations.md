# Koinon UI foundations: research and storyboards

- Status: Complete
- Date: 2026-10-22
- Area: frontend-ux
- Tags: ui, approvals, chat

Date: 2026-10-22
Status: research note. Input to the v0.2 design pass (issue #91) and to the v0.1 UI follow-ups.

## Summary

Koinon's interaction model is already sound. ADR 0017 says the board is home and every conversation is a ticket. ADR 0018 fixes nine screens in one SPA with role-gated access. ADR 0008 and ADR 0042 say an approval is bound to the exact request and the run parks until a human decides. The research mostly confirms those choices and sharpens six points.

1. **The approval card is the product's trust surface. Optimise it first.** Evidence on warning habituation and automation bias says people rubber-stamp frequent, low-variance prompts. So approvals should be rare (good presets), specific (exact request, consequences first) and cheap to decide well (diff, risk facts, undo notes). v0.1's risk-flag approach, which leads with what runs in CI, is the right shape.
2. **Use conventional navigation.** Jakob's Law applies fully. A left sidebar, a board, a command palette (Cmd+K), an inbox-like `Waiting on you` view and breadcrumbs match what users of GitHub, Linear and Slack already know. Do not invent a layout.
3. **Chat belongs inside the ticket, with structured cards in the stream.** This is ADR 0017. The research says chat-only UIs hurt structured tasks, so approvals, diffs, plans and tool calls must be cards, not prose.
4. **Phones need three things: decide an approval, read and answer a ticket thread, and see what is waiting.** Everything else can be desktop-first. Approval must work as a full-screen sheet with 44 px targets and a notification deep link.
5. **Do not build separate apps per role.** One shell, with navigation items and home content that vary by role. Tiers differ in density and in which screens exist, not in layout.
6. **Measure cheaply from day one.** Event logs on approval cards (shown, expanded, decided, outcome) give most of the agent-specific metrics at no extra cost. SUS and a 5-task test give the rest.

Biggest gaps: no high-trust quantitative source exists for onboarding drop-off in developer tools, none for "AI sparkle" overuse, and I could not verify first-party pattern details for Paperclip. See Evidence gaps.

Claim labels: **established** (several independent sources, or a standard), **reported** (one study or a first-party description), **inference** (my reasoning from adjacent evidence).

## What exists today

**Product decisions (docs/adr on origin/docs/plan-v0.1).**

- ADR 0017: the board is home, `waiting on you` is a first-class column, a conversation is a ticket, Slack/Teams threads mirror tickets (v0.2).
- ADR 0018: nine screens (Board, Ticket, Agents, Agent editor, Connections, MCP servers, Org settings, Platform admin, Audit). Every user gets an automatic personal org. v0.1 ships a subset: board, ticket view, agent from template, approval card.
- ADR 0020: agents are always created as drafts. Only an org admin or owner activates and approves grants. The builder agent proposes and never grants.
- ADR 0019: a personal agent may call a team agent if its owner is a member of the team's org. The effective grant is the intersection of the team agent's exposure profile and the user's role. Read tier only by default. Approvals go to the team org's approvers.
- ADR 0011: every A2A task is a child ticket with a depth and a parent cost link. Results return as quoted data.
- ADR 0008 and ADR 0042: tiers `read`, `reversible-write`, `external-or-irreversible`, `forbidden`. Presets `Read only`, `Standard`, `Trusted`. An ask-tier call parks the run. Koinon executes the exact approved call and resumes.
- ADR 0004: roles live in Postgres. Role names seen in the ADRs: owner, org admin, `platform-admin` (above all orgs). A plain member role is implied but I did not find it defined (see Decisions).
- ADR 0022: React SPA, Vite, connect-rpc, TanStack Router/Query/Table/Virtual, shadcn/ui. Server-streaming events come from a Postgres outbox with resume by last-seen ID.
- Issue #91 (v0.2): persistent coworkers. Hermes as a chat-first personal agent. Evolving persona (fixed Koinon core plus an agent-edited persona, versioned and revertible). Memory private per requester. The exact UI is left to a v0.2 design pass.

**v0.1 web app (origin/feat/approval-diff, `web/`).**

- Stack: React 19, Vite 8, TypeScript 6, Tailwind 4, shadcn/ui on radix-ui, lucide icons, TanStack Router and Query, connect-query, Vitest, Playwright, oxlint.
- Routes: `/login`, `/` (board), `/tickets/$ticketId`, `/approvals/$approvalId`, `/agents`, `/agents/new`, `/agents/$agentId`, `/connections`, `/connections/new/github`, `/connections/$connectionId`.
- Components: board with four columns (grid, 1/2/4 columns responsive) and a `Waiting on you` list fed by approvals the viewer can decide; ticket timeline; create-ticket form; approval card with a diff and risk flags (CI config, scripts, executable bit, dependency manifests, binaries, deletions, size) and an optional note; agent-from-template form (`agent-wizard.tsx`, one page: template, name, flavor, grants with a repo picker); GitHub connection form with setup guide, secret textarea and repo picker.
- Behaviours already good: the approval is refused with a "changed since loaded" state (HTTP 409) that forces re-review; `canDecide` gates the buttons on role; columns use headings with `aria-labelledby`.
- Missing in the shell: no sidebar or command palette seen, no mobile-specific layout beyond the responsive grid, no chat composer, no streaming run view with tool calls, no hierarchy view, no wizard steps, no notification settings.

This document marks each storyboard element as **[v0.1]** (exists), **[new]** (v0.1 or v1.0 work not yet built) or **[v0.2]**.

---

## 1. Usability metrics worth tracking

**Principle (established).** Pick few metrics tied to goals. Google's HEART framework (Rodden, Hutchinson and Fu, CHI 2010) pairs five categories (Happiness, Engagement, Adoption, Retention, Task success) with a Goals, Signals, Metrics process so that each metric has a reason to exist. Koinon is small and self-hosted, so most measurement must be local, opt-in and privacy-respecting. Self-hosters will not accept silent phone-home telemetry (**inference**). Default to events stored in the instance's own Postgres and viewable by org admins and the platform admin.

| Metric | What it tells you | Cheap way to measure |
| --- | --- | --- |
| Task success | Can a new user complete the core path? | Moderated test, 5 users, five tasks (connect GitHub, create agent, create ticket, decide approval, find the resulting PR). Binary success plus notes. NN/g's long-standing guidance is that about five users find most usability problems in one round; iterate rather than run one large study (**established**, Nielsen, "Why You Only Need to Test with 5 Users"). |
| Time on task | Efficiency and friction | Same sessions, a stopwatch. In product: time from `install complete` to `first approved PR` as a funnel from server events (**inference**). |
| Error rate | Slips and mistakes | Count wrong clicks, form validation failures per field (log field name only), 409 "changed since loaded" events, and approvals reversed within N minutes. |
| SUS | Overall perceived usability | 10-item System Usability Scale (Brooke, 1996), run after the test session. A score near 68 is commonly cited as the average (**reported**, from Sauro's benchmark work; treat as a rough anchor, not a target). Needs about 10+ respondents to be stable. |
| HEART | Product-level dashboard | Happiness: SUS plus one in-app question after the first approved PR. Engagement: weekly approvals decided and tickets created per active org. Adoption: orgs with at least one active agent. Retention: orgs active in week 4. Task success: the funnel above. Pick 1 signal per category. |
| Approval decision time | Are people reading the card? | Log `card_visible` (IntersectionObserver) and `decided` timestamps. Report median and the share of decisions under 3 s with the diff unexpanded (a rubber-stamp indicator; the 3 s threshold is **inference**, tune it against sessions). |
| Override and reject rate | Is the agent's request quality good, and are presets right? | Approvals denied / total, per agent, per tier, per preset. A near-zero reject rate on a `Standard` preset can mean good presets or rubber-stamping, so read it with decision time. A high rate means the preset is too loose or the agent is poorly instructed. |
| Trust calibration | Do people trust the agent about as much as it deserves? | Join each approval to its outcome: approved and later reverted or closed unmerged, versus denied. Add a one-question survey per week ("how much did you verify before approving?"). Calibration is the match between trust and capability (Lee and See, 2004, **established** as a concept). Over-trust (misuse) and under-trust (disuse) are both failures (Parasuraman and Riley, 1997). |
| Interruption cost | Does the system break people's focus? | Count human-facing interrupts per user per day (approvals, `input-required`, push, Slack posts). Measure notification-to-action latency. Add a survey item about whether prompts felt unnecessary. Interrupted work is completed faster but with more stress (**reported**, Mark, Gudith and Klocke, CHI 2008). |
| Intervention rate | Is autonomy right-sized? | Runs where a human stopped, edited or took over, divided by runs. |
| Time to first value | Onboarding | Minutes from first login to first agent PR (**inference**; ADR 0036 targets a 15-minute quickstart). |

**Reading them together (inference).** Fast decisions with a low reject rate, a low verify score and a later reverts count mean rubber-stamping. Fast decisions with a low reject rate and no reverts mean the preset is working. Report these as a pair, never alone.

---

## 2. Navigation

**What is established.**

- Jakob's Law: users spend most of their time on other sites, so they expect yours to work the same way (Nielsen, "The Need for Web Design Standards", NN/g, https://www.nngroup.com/articles/the-need-for-web-design-standards/; NN/g presents Jakob's Law mainly as a video). **Established**: the principle is widely documented.
- Recognition over recall is Nielsen's heuristic 6 (NN/g, "10 Usability Heuristics for User Interface Design"). Visible navigation labels beat hidden commands. **Established.**
- Information scent: people follow cues (labels, snippets) that predict where a path leads (Pirolli and Card, information foraging; NN/g, "Information Scent"). Labels must be specific words, not clever ones. **Established.**
- Persistent global navigation, current-location marking and breadcrumbs are staples of NN/g navigation guidance. Breadcrumbs help in deep hierarchies and are secondary to the main nav (NN/g, "Breadcrumbs: 11 Design Guidelines"). **Established.**
- Material 3 and Apple HIG both recommend a bottom navigation bar or tab bar with 3 to 5 top-level destinations on phones, and a side rail or sidebar on larger screens. **Established.**

**What users of the reference tools already expect (reported; each is first-party product behaviour).**

- GitHub: a top bar with global search and a per-repo tab strip (Code, Issues, Pull requests, Actions, Projects), a command palette on Cmd+K, a notifications inbox, breadcrumb-style repo path. Projects offer board, table and roadmap views over the same items.
- Linear: a left sidebar with Inbox, My issues, Teams, Views; Cmd+K for commands and search; strong keyboard shortcuts; triage inbox; one item opens in a side panel or full page.
- Slack: left rail of workspaces, a sidebar of channels and DMs, a Threads view, an Activity (mentions and reactions) view, Cmd+K quick switcher.

**Recommendation for Koinon (inference, anchored on the above).**

- **Left sidebar on desktop** with an org switcher on top. Items: Board (home), Waiting on you (with count), Agents, Connections, MCP servers, then Org settings, Audit. A "Personal" entry for the personal org's board and agent. Role-gated items simply do not render (see section 8).
- **Top bar**: breadcrumb (Org / Board / Ticket 142), global search and command palette trigger, notification bell, user menu. The palette (Cmd+K) is an accelerator and must never be the only route to anything (recognition over recall).
- **Search scope**: tickets, agents, connections, people. Results grouped by type, each with a one-line snippet and status, because snippets are information scent.
- **Switch orgs rarely, switch views often**: a personal user has one org; a team user has two or three. The org switcher should be a plain dropdown, not a hidden menu.
- **URL per object** (already so in v0.1 routes). Deep links are what makes Slack, push and email notifications work.
- **Slack and Teams threads** are a navigation surface too (ADR 0017). The ticket page must therefore show "mirrored to #channel" and link back.

**What breaks (inference).** Overloading the sidebar with every ADR screen (nine plus agent children). Keep the sidebar to six or seven items; move Audit, MCP servers and Org settings under a "Settings" group.

---

## 3. Visualising hierarchy and relationships

Koinon has four relationship types: org contains teams contains agents (containment), user owns personal agent (ownership), personal agent calls team agent (delegation, ADR 0019), and ticket parent/child via A2A (call chain, ADR 0011).

| Representation | Works when | Breaks when | Evidence |
| --- | --- | --- | --- |
| **List with badges** | Up to dozens of items, tasks are "find an agent", "see status", "who owns this" | Relationships are the point (who calls whom) | Lists are scannable and accessible; NN/g and Material treat them as the default for collections (**established**). |
| **Tree (outline)** | Strict containment: org, team, agent. Depth 3 or less | Many-to-many (an agent in two teams, a link across orgs) | The ARIA Authoring Practices treeview is a known accessible pattern; use it only for true trees (**established**). |
| **Org chart (box tree)** | Showing reporting lines to non-technical audiences | More than about 30 nodes, or any non-tree edge; wide charts need pan and zoom, which is poor for phones and screen readers | Established as a convention; large charts are hard to use (**inference**). |
| **Node-link graph** | Small graphs (tens of nodes) where paths matter, e.g. one delegation chain | Dense graphs hairball; reading edges costs effort | Ghoniem, Fekete and Castagliola found matrices beat node-link diagrams above about 20 vertices, while node-link is better for path-finding (**reported**, Information Visualization, 2005). |
| **Matrix** | "Which agent may call which", i.e. the access matrix | Unfamiliar to many users | Same study. Good for an admin-only permissions overview (**inference**). |
| **Treemap** | Showing budget or cost shares | Not for relationships | Shneiderman, 1992 (**established**). |

**Recommendations (inference).**

1. **Default Agents screen: a list grouped by team, with badges** for status (draft, active, archived), current run, flavor, budget use and "accepts personal agents". This matches ADR 0018's Agents screen and scales.
2. **Optional "Structure" tab: an indented tree** org, teams, agents, with counts, for admins who want the shape. Collapsible, keyboard-operable, and never the only view.
3. **Delegation chains live on the ticket, not in a global graph.** Show a breadcrumb-like strip: `You > Hermes (personal) > Billing-analyst (Team Finance) > git-agent`. Each hop is a link to a child ticket (ADR 0011 already persists each as a ticket). Depth is capped by the platform, so a strip never grows unbounded.
4. **Attribution everywhere, in plain text.** Every timeline event carries "who": person, agent, or agent acting for person. Use the form `Hermes, on behalf of Maria` (the exact wording is a Decision). Cost and approvals roll up to the parent ticket (ADR 0011, ADR 0041).
5. **Cross-org links** (ADR 0012) appear as a labelled chip on the agent ("provided by Org X, you pay"). Never draw them in a graph in v1.
6. **At scale**, add search and filter first, then grouping by team, then pagination or virtual lists (ADR 0022 already includes TanStack Virtual). A graph view is a v1.x question once real orgs exist.

---

## 4. Chatting with an agent

**Evidence and conventions.**

- **Response time.** About 0.1 s feels instant, 1 s keeps the flow of thought, 10 s is the limit of attention (Nielsen, "Response Times: The 3 Important Limits"; **established**). Agent runs take minutes, so the UI must show progress, not a spinner. Streaming text and a visible step list serve that need.
- **Make capability and reasoning visible.** Amershi et al. (CHI 2019), "Guidelines for Human-AI Interaction", has 18 guidelines. Most relevant: G1 make clear what the system can do; G2 make clear how well it can do it; G3 time services based on context; G9 support efficient correction; G10 scope services when in doubt; G11 make clear why the system did what it did; G17 provide global controls. **Established**.
- **Google PAIR People + AI Guidebook** (https://pair.withgoogle.com/guidebook). Chapter names not verified; in general it covers mental models, explainability and trust, feedback and control, and errors. It advises setting expectations early, explaining in terms of what users need to decide, and giving control (**established** as a high-trust practitioner source, not an experiment).
- **Chat is a poor fit for structured tasks.** Mugunthan argues that prompt-driven UIs have an "articulation barrier" (users must write well) and that hybrid text plus GUI interfaces help (Tarun Mugunthan, "Overcoming the Articulation Barrier in Generative AI Using Hybrid Interfaces", NN/g, 13 Aug 2023, https://www.nngroup.com/articles/ai-articulation-barrier/; **reported**, expert analysis rather than a study). Subramonyam et al. (CHI 2024, doi 10.1145/3613904.3642754) describe a "gulf of envisioning" between what users can express and what the model needs (**reported**; the description of the gulf is inference, unverified).
- **Agent autonomy levels.** Feng, McDonald and Zhang propose five levels of user involvement with an agent (arXiv 2506.12469, 2025; five levels confirmed; the role names operator, collaborator, consultant, approver, observer are unverified; **reported**, peer review not verified). Parasuraman, Sheridan and Wickens (2000) set out levels of automation and stages (acquire, analyse, decide, act) (**established**). Koinon's presets (`Read only`, `Standard`, `Trusted`) are a coarse level-of-automation control; show the current preset on the agent.
- **Interrupting a run.** Users need a visible Stop that works at any time (Amershi G17, G9). ChatGPT, Claude.ai and Cursor all expose a stop button during generation, and Cursor and Claude Code keep a way to rewind to a checkpoint (**reported**, first-party product docs and behaviour; reported, not re-checked).
- **Tool calls inline.** Claude Code, Cursor and the GitHub Copilot coding agent display tool calls and file edits in the stream, collapsed by default with expandable detail, and Copilot's coding agent keeps a session log linked from the PR (**reported**, first-party docs). Devin shows a timeline of plan, shell, browser and editor activity next to chat, and integrates with Slack (**reported**; reported, not re-checked). The Copilot and Claude Code claims in this item are likewise reported, not re-checked.
- **Citations.** ChatGPT search and Claude.ai web search attach numbered source links to claims (**reported**; reported, not re-checked). For Koinon, an agent's claims about a repo should link to the file and line, or the ticket, or the memory entry, that supports them (**inference**).

**Design rules for the Koinon ticket thread (inference built on the above).**

1. **One timeline, typed events.** Message, plan, tool call (collapsed), file edit (diff), approval card, A2A hop, artifact, cost tick. Messages are prose; everything with structure is a card.
2. **Streaming text is bounded.** Stream into a message bubble but cap visible height with "Show more" and auto-collapse finished tool output. Never auto-scroll if the user scrolled up (show a "N new events" jump pill).
3. **Progress is a live plan.** Show the agent's checklist (done, doing, todo) at the top of the run. Each step links to its tool calls.
4. **Stop is always visible** in the composer area while a run is active, and says what happens: "Stop run (work so far is kept)". Parked runs show "Waiting for X".
5. **Run state is text plus icon**, not colour alone: Running, Parked (waiting on approval), Needs input, Done, Failed, Stopped. These map to ADR 0030's state machine.
6. **Composer** supports plain text, attach, and `@agent` mention. A mention does not create a new chat; it addresses the ticket's agent or adds another agent to the ticket.
7. **Conversation versus ticket thread.** In Koinon they are the same object (ADR 0017). The cost is "small talk creates tickets" (ADR 0017 itself notes this). Mitigation: a "Chat" ticket kind that is auto-archived when idle and hidden from the board by default, with a "promote to task" action. This preserves one data model.
8. **Identity and persona without manipulation.**
   - Show the agent's name, role in one line, flavor, the model/runtime and who owns it. A fixed "AI agent" label stays on every agent message; do not allow personas to remove it. The EU AI Act's transparency obligation (Article 50) requires that people be told when they interact with an AI system (**established**, regulation; check scope for your deployment).
   - The persona work in `docs/research/persona-evolution.md` finds that persona prompts do not reliably improve accuracy, that models drift within a conversation, and that self-edited personas show "misevolution" risk unless limited to descriptive fields. The UI consequence (**inference**): show persona as editable, versioned text on the agent page ("Style and working notes", with history and revert), keep role, rules and permissions in a separate read-only "Core" panel, and make persona edits appear as reviewable diffs in the timeline.
   - Do not use human avatars, typing-delay theatrics, or first-person emotional claims ("I'm so excited"). Use a neutral glyph or initial. Warmth belongs in word choice, not in simulated feelings. See section 10.
9. **Why did it do that?** Every action card has an "Explain" disclosure: the instruction or ticket text, the tool, the arguments (Amershi G11). Do not present model-generated rationales as ground truth (explanations can raise acceptance regardless of correctness; Bansal et al., CHI 2021, **reported**).

---

## 5. Approval cards

**What the evidence says.**

- **Human-in-the-loop is not a safeguard by itself.** Bainbridge's "Ironies of Automation" (1983) shows that monitoring tasks degrade human vigilance. Parasuraman and Riley (1997) document misuse (over-reliance) and disuse. Automation bias is well documented in clinical decision support and aviation (**established**).
- **Warning habituation.** Repeated, similar warnings are ignored. Egelman, Cranor and Hong (CHI 2008, "You've Been Warned") found that active (interruptive) warnings were heeded far more than passive ones (79% versus about one participant). Akhawe and Felt (USENIX Security 2013, "Alice in Warningland") measured large-scale click-through of browser warnings in the wild and found it varied sharply by warning design (70.2% for Chrome SSL warnings versus about 10% for Firefox malware warnings). Anderson et al. (CHI 2015, pp. 2883-2892) showed neural habituation to repeated security warnings with fMRI (**reported**, all three). Clinical alert fatigue shows the same: about a quarter of drug alerts were repeats, and acceptance fell with complexity and repetition (Ancker et al., 2017, BMC Medical Informatics and Decision Making 17:36, doi 10.1186/s12911-017-0430-8; **reported**). Together this is **established** as a pattern.
- **Cognitive forcing.** Buçinca, Malaya and Gajos (CSCW 2021, PACM HCI 5(CSCW1) art. 188, doi 10.1145/3449287, "To Trust or to Think") found that forcing a decision step before showing the AI's suggestion reduced over-reliance but users liked it less and it helped users high in Need for Cognition most (**reported**). So friction should be targeted at high-risk cards, not all cards.
- **Explanations are not a cure.** Bansal et al. (CHI 2021) found AI explanations increased acceptance of AI advice whether or not it was right (**reported**). So show facts about the action (what changes, where, what runs), not persuasive reasoning.
- **Updates in human–AI teams.** Bansal et al. (AAAI 2019) show that a model update that breaks the user's mental model can reduce team performance even if accuracy rises (**reported**). It supports showing agent version changes ("this agent's flavor was updated") on the card.
- **Product patterns.** GitHub's PR review lets reviewers batch comments in a pending review, then submit with Approve, Comment or Request changes. Files can be marked "viewed" (**reported**, GitHub docs). Claude Code and Cursor ask per tool call with allow-once or allow-always. ChatGPT agent asks for confirmation before consequential steps and offers takeover for sensitive inputs (**reported**, first-party docs; reported, not re-checked).

**Design rules for the Koinon approval card (inference unless noted).**

1. **Lead with consequences, in plain words.** Headline: "Maria's agent wants to open a pull request in acme/api that changes 3 files (+84 −12)." Then the risk facts. v0.1 already leads with what runs in CI, scripts, executable bits, dependency manifests, binaries, deletions and size. That is the right facts-first approach.
2. **Tier and reversibility shown as text.** "Reversible write" or "External or irreversible", with one line on how to undo ("Close the PR; nothing is merged"). ADR 0008's tiers map straight to a label. Never colour alone (section 10).
3. **Bound to the exact request.** Show the request hash or version. If anything changes, block with the "changed since you opened this" state and force re-review (v0.1 already does this with 409).
4. **Diff presentation.** Default: file list with per-file +/− counts, risky files pinned first and auto-expanded, others collapsed. Unified diff on phones, split on wide screens. "Mark as viewed" per file as on GitHub. Cap size and say so ("100-file cap").
5. **Decision controls.** Approve and Deny, equal in size, labelled with the verb and object ("Approve pull request", "Deny"). Optional note (exists in v0.1). A required note on deny is cheap but adds friction; recommended only for `external-or-irreversible`.
6. **Batch approvals: allow only for identical-risk, low-tier items, never for irreversible ones.** Batch with a summary table and a per-row expand. The review evidence for habituation says the larger the batch, the more it turns into a single click; keep batch off for the top tier (**inference**).
7. **Fatigue controls come from the preset, not the card.** Reduce volume by raising the preset for proven agents ("Trusted") and by learned-rule suggestions (ADR 0008). Show "You have decided 14 approvals today from this agent; consider a narrower rule" (**inference**).
8. **Undo.** After approve, show "Pull request #88 opened. Undo: close it" and the execution result (ADR 0042: Koinon runs the exact call with an idempotency key). Real undo is possible only for reversible tiers; for irreversible ones say "cannot be undone" before, not after.
9. **Two-person rule** (ADR 0008): show "1 of 2 approvals" and who decided. Do not show a non-approver a live Approve button (v0.1's `canDecide` does this).
10. **Parked run clarity.** On the card and on the ticket: "The agent is paused until you decide."
11. **Timeouts.** Say what happens if no one decides (the run stays parked; cost stops). Do not auto-approve or auto-deny silently (open question; Decision 9).

---

## 6. Wizard for adding a new agent

**What the evidence says.**

- NN/g's wizard guidance: wizards suit infrequent, unfamiliar tasks with ordered dependencies; they hurt frequent tasks and expert users, because they force linear flow and hide the whole (NN/g, "Wizards: Definition and Design Recommendations"; **established** practitioner guidance). Creating an agent is infrequent for most users and has real dependencies (connection before grant before repo), so a wizard fits. The v0.1 single-page form is fine for one template and may be right for repeat users (offer a "Quick create" path).
- GOV.UK's "one thing per page" and "Check answers" patterns: ask one question per step, let users review and change before submitting (**established**, design-system patterns). GOV.UK's task list pattern shows status per step.
- Progressive disclosure: show essentials first, advanced options on demand (NN/g, "Progressive Disclosure"; **established**).
- Defaults matter. Most users keep defaults, so defaults must be safe (the NN/g and behavioural economics literature; **established**). For Koinon: Read only preset by default, a small budget, no schedule.
- Validation timing: validate on blur for fields with a format (repo patterns), not on every keystroke, and keep errors next to the field with text, and summarise at the top on submit (GOV.UK error pattern; WCAG 3.3.1, 3.3.3). Wroblewski's inline-validation study reported better completion for inline validation of specific fields (**reported**, an A/B test published on his blog, not peer reviewed; reported, not re-checked).
- Saving drafts: ADR 0020 already makes every result a draft. So there is no "save progress" worry: the wizard creates a draft at step 2 and every later step edits it (**inference**). Resume from the Agents list.

**Recommended wizard (inference).** Four steps, templates first, review last.

1. **Start**: pick a template (cards with name, one-line purpose, flavor, tier of what it can do) or "Describe it to the builder" (ADR 0020). Pre-selects defaults.
2. **Basics**: name, one-line role, flavor (collapsed under "Advanced" unless the template leaves it open).
3. **Access**: grants. For each, pick a connection (or "Connect GitHub" inline, which returns here with state kept), a preset, and repositories (v0.1 repo picker). Show the plain-language summary: "Can read code in acme/api. Needs your approval to open pull requests."
4. **Limits and review**: budget and schedule (collapsed defaults), then a summary page listing everything, with "Change" links per row. Primary button: "Create draft". Next screen explains: "An admin must activate this agent" (ADR 0020) and who that is.

Rules: step indicator with names, not just numbers; Back never loses data; no required fields beyond name and one grant; the Review step is mandatory because grants are security-relevant; do not ask for the same thing twice (WCAG 3.3.7 Redundant Entry).

---

## 7. Mobile layout

**Evidence and standards.**

- WCAG 2.2 SC 2.5.8 Target Size (Minimum), AA: pointer targets at least 24 by 24 CSS px, with exceptions. Apple HIG recommends 44 by 44 pt, and Material recommends 48 by 48 dp (**established**). Aim for 44 px on touch-primary controls.
- WCAG 1.4.10 Reflow (AA): content works at 320 CSS px width without two-dimensional scrolling. WCAG 2.5.7 Dragging Movements (AA, new in 2.2): anything dragged (board cards) needs a non-drag alternative (**established**).
- Material 3 and HIG: bottom navigation or tab bar with 3 to 5 destinations; modal sheets for focused tasks (**established**).
- Responsive versus adaptive: responsive (fluid, one codebase, breakpoints) is the default for web; adaptive (separate layouts per class) only when tasks differ (**established** practitioner consensus; Material's window size classes formalise compact, medium, expanded). Koinon is one SPA (ADR 0022), so use responsive layout with three window classes.
- Notifications: users lose trust in apps that notify too often; Slack, GitHub and Linear all offer per-type controls and a digest (**reported**, product behaviour; reported, not re-checked). Push needs a PWA on iOS (Web Push on iOS works for home-screen web apps since iOS 16.4; **reported**, Apple release notes; reported, not re-checked). Slack and Teams mirroring (ADR 0017) is the pragmatic notification channel in v0.2.

**What must work on a phone (inference).**

1. See `Waiting on you`, with count.
2. Open one approval and decide it (full-screen sheet, section storyboard).
3. Read a ticket thread and send a reply or answer an `input-required` question.
4. Stop a run.
5. For personal agents (v0.2): chat. This is the primary mobile surface for Hermes.

**Desktop-first (inference):** Agent editor, wizard Review step is fine on phone but long grants editing is not; Connections setup (secrets, long guides); MCP servers; Org settings; Audit; hierarchy Structure tab. They must not break at 320 px, but they need not be pleasant.

**Layout.** Bottom bar with four items: Board, Waiting (badge), Agents, More. Board columns become a segmented control or horizontally swipeable tabs with a "Move to" menu instead of drag. Ticket view is a single column with the composer pinned above the keyboard. Approvals open as a full-screen sheet with sticky Approve/Deny at the bottom within thumb reach.

**Notifications (inference).** Default on: approval requested, input needed. Default off: run finished, comments. One digest option. Each notification deep-links to the sheet and states the consequence ("Approve opening PR in acme/api?"). No notifications for agent-to-agent hops.

---

## 8. Layouts per user tier

**Roles found.** ADR 0004: org roles live in Postgres; every org has at least one manually assigned owner; `platform-admin` sits above all orgs; membership mode is `platform`, `idp` or `hybrid`. ADR 0020: org admin or owner activates agents. ADR 0018: automatic personal org with one owner. v0.1 web: only admins and owners may decide approvals and list repositories. A plain "member" role is implied (someone who uses agents but cannot activate them) but I did not find it defined; confirm it (Decision 3).

| Tier | Needs | Home | Density | Nav items |
| --- | --- | --- | --- | --- |
| Member | Hand work to agents, follow tickets, answer questions, see approvals they requested | Board, filtered "mine" with `Waiting on you` (for clarifications) | Medium; plain language | Board, Waiting on you, Agents (read-only) |
| Org admin / owner | All of the above plus decide approvals, activate drafts, connections, members, budgets | Board with `Waiting on you` leading with approvals and drafts to activate | Medium-high; tables for agents, members | Plus Connections, MCP servers, Org settings, Audit |
| Platform admin | Orgs, connector types, auth types, global policy, health | Platform overview: orgs, spend, failures, policy | High; tables, filters | Platform admin section, kept visually separate; also all org screens when acting inside an org |
| Personal-only user | One org with one owner; one personal agent; chat | Personal home: chat with the personal agent, `Waiting on you`, recent tickets | Low; fewest items | Chat/Board, Agents, Connections (their own), Settings |

**Principles.**

- **One shell, role-aware content.** ADR 0018 already rejects a separate admin console. NN/g and Material guidance on consistency favour one navigation model with items shown by permission (**established** for consistency; role-based nav is **inference**).
- **Hide what the user cannot use, but explain once.** Disabled controls with a reason ("Only admins can activate agents") beat silently missing ones for actions the user might expect; entirely irrelevant screens (Platform admin) are hidden.
- **Personal org gets the same screens with lighter defaults.** A personal user is an owner, so they see everything, but the sidebar groups configuration under one "Settings" item and the home is chat-first (v0.2). That respects ADR 0018 (no special code path) while changing only defaults (**inference**).
- **Platform admin is a mode, not an app.** A clearly marked area plus an org switcher when entering an org, with a persistent banner "Acting as platform admin in Org X" to prevent confusion and wrong-org actions (**inference**).

---

## 9. Process flows and drop-off

Core path: install, login, connect GitHub, create agent, first ticket, approval, PR (storyboard at the end).

**What the evidence says about drop-off.**

- The honest finding is that I found no high-trust, published, quantitative benchmark for developer-tool onboarding drop-off by step. Vendor blog statistics exist (activation rates, tutorial skips) but are not reliable enough to cite.
- Qualitative, high-trust guidance agrees on where onboarding fails: long forms before value, unexplained credentials and permissions, and empty states with no next step (NN/g, "Onboarding Tutorials vs. Contextual Help"; NN/g on empty states; Google PAIR, Onboarding chapter) (**established** as practitioner guidance). Baymard's checkout research is e-commerce, but its robust finding that every extra form field and forced account creation costs completions transfers as direction, not as numbers (**reported**; **inference** for transfer).
- Developer tools with the closest flows: GitHub Apps installation (pick account, pick repos, approve scopes), then returning to the product. The user leaves the product and must come back; this is the most likely drop-off (**inference**).

**Likely drop-off points for Koinon (inference) and mitigations.**

| Step | Risk | Mitigation |
| --- | --- | --- |
| Helm install | Cluster and chart issues, outside the UI | Quickstart script, health page with plain checks; target the 15-minute quickstart (ADR 0036) |
| Login (OIDC) | No IdP set up (ADR 0004 open question: bootstrap admin) | A single bootstrap admin for first run, with a banner to add OIDC |
| Connect GitHub | Leaving the product; creating a GitHub App; pasting secrets; wrong repo scope | Step-by-step guide in-page (v0.1 has a guide), a progress checklist, return to the same step with state saved, minimum scopes shown in plain words |
| Create agent | Choice overload; unclear what grants mean | Templates first; safe defaults; plain-language grant summary; review step |
| Draft not active | User thinks they are done but the agent cannot run (ADR 0020) | Clear state "Draft: waiting for an admin to activate"; if the user is the admin, offer "Activate now" on the next screen |
| First ticket | Blank board, no idea what to type | A starter ticket suggestion from the template ("Fix a failing test in acme/api") |
| Approval | Surprise: the run parks and nothing seems to happen | Say on the ticket "Paused: waiting for your approval", push or Slack nudge, and a `Waiting on you` count |
| PR | Result not obvious | Link to PR on the card and ticket; "What next" row (review in GitHub, merge, give feedback) |

Use a visible **setup checklist** on the home screen until the first PR (GOV.UK task list pattern; GitHub and Linear use similar getting-started cards, **reported**). Measure each step as a funnel event (section 1).

---

## 10. Anti-patterns to avoid

| Anti-pattern | Why | Source | Koinon rule |
| --- | --- | --- | --- |
| **Dark patterns** (nudging, confirmshaming, pre-ticked consent, hidden costs) | Manipulate users into choices against their interest | Gray et al., CHI 2018, "The Dark (Patterns) Side of UX Design"; Mathur, Acar, Friedman, Lucherini, Mayer, Chetty and Narayanan, CSCW 2019, "Dark Patterns at Scale" (doi 10.1145/3359183); Brignull's catalogue (deceptive.design); EU Digital Services Act Art. 25 (**established**) | Approve and Deny equal weight; no pre-set "always allow"; budgets and costs visible; no guilt wording on deny |
| **Hidden agent actions** | Users cannot oversee what they cannot see | Amershi et al. G11, G17 (**established**); Google PAIR Explainability + Trust | Every tool call, A2A hop and cost is a timeline event; the Audit screen is the backstop |
| **Over-automation without visibility** | Out-of-the-loop problem and complacency | Bainbridge 1983; Parasuraman and Riley 1997; Parasuraman, Sheridan and Wickens 2000 (**established**) | Presets visible; a "what this agent can do without asking" summary on each agent; periodic review prompts for `Trusted` agents |
| **Modal overload** | Interrupts flow, blocks, inaccessible if poorly done | NN/g, "Modal and Nonmodal Dialogs: When (and When Not) to Use Them" (**established**); WCAG 2.4.3, 2.4.11 for focus | Approval is a page or sheet, not a stack of modals; no confirm-on-confirm; use inline status and undo instead of "Are you sure?" except for irreversible tiers |
| **Notification spam** | Habituation and interruption cost | Iqbal and Horvitz, CHI 2007; Mark et al., CHI 2008 (**reported**) | Defaults limited to approvals and input needed; digest; per-agent mute |
| **Chat-only UI for structured tasks** | Articulation barrier; hard to review structured results | Mugunthan, NN/g 2023 (**reported**); Subramonyam et al. CHI 2024 (**reported**) | Forms, cards and diffs for approvals, grants, wizard; chat for intent and clarification |
| **Unbounded streaming walls of text** | Cost of reading, loss of place | Nielsen response-time guidance (**established**); practice in Claude.ai, ChatGPT and Cursor of collapsing tool output (**reported**) | Cap height, collapse finished steps, sticky summary, no forced auto-scroll |
| **Fake human typing; personas claiming feelings** | Deception about the nature of the system; inflated trust | EU AI Act Art. 50 (**established**); Shanahan et al. 2023 on role play; `persona-evolution.md` on drift and sycophancy risk | Fixed "AI agent" label; no typing-delay theatre; no emotional first-person claims in the Core; persona limited to descriptive style |
| **"AI" sparkle overuse** | Decoration that signals nothing and distracts | No primary study found. **Inference** from NN/g's "aesthetic and minimalist design" heuristic and from the lack of function in the icon | Label by function ("Draft with builder"), not a sparkle icon; use one consistent agent glyph only to mark agent-authored content |
| **Inaccessible colour-only risk signals** | About 8% of men have colour vision deficiency; colour alone fails WCAG | WCAG 2.2 SC 1.4.1 Use of Color (A), 1.4.11 Non-text Contrast (AA) (**established**) | Risk tier is text plus icon plus colour; 3:1 contrast for icons, 4.5:1 for text |

---

## 11. Accessibility baseline (WCAG 2.2 AA)

Items most relevant to Koinon. All **established** (W3C standard). The practical mapping is **inference**.

| SC | Level | What it means here |
| --- | --- | --- |
| 1.1.1 Non-text Content | A | Icons for risk, status and agent glyphs have text alternatives; diffs are text, not images |
| 1.3.1 Info and Relationships | A | Board columns, timeline events and diff files use real headings, lists, tables (v0.1 columns already use `aria-labelledby`) |
| 1.4.1 Use of Color | A | Risk and status never by colour alone |
| 1.4.3 Contrast (Minimum) | AA | 4.5:1 text; check diff green/red and muted text (shadcn defaults need checking in dark mode) |
| 1.4.10 Reflow | AA | 320 px without horizontal scroll, except for diffs and code, which may scroll in a region |
| 1.4.11 Non-text Contrast | AA | 3:1 for borders of inputs, icons and focus rings |
| 1.4.13 Content on Hover or Focus | AA | Tooltips dismissable and hoverable |
| 2.1.1 Keyboard, 2.1.2 No Keyboard Trap | A | Everything operable by keyboard, including board moves and the command palette |
| 2.4.3 Focus Order, 2.4.7 Focus Visible | A, AA | Predictable order in sheets and cards |
| 2.4.11 Focus Not Obscured (Minimum) | AA | Sticky headers and the mobile Approve bar must not cover focus |
| 2.5.7 Dragging Movements | AA | Board drag has a "Move to" menu |
| 2.5.8 Target Size (Minimum) | AA | 24 by 24 CSS px minimum; use 44 on touch |
| 3.2.6 Consistent Help | A | Help link in the same place on every page |
| 3.3.1 Error Identification, 3.3.2 Labels or Instructions, 3.3.3 Error Suggestion | A, AA | Wizard and connection form errors in text next to the field and in a summary |
| 3.3.4 Error Prevention (Legal, Financial, Data) | AA | Review-before-create, undo for reversible actions, confirmation for irreversible ones |
| 3.3.7 Redundant Entry | A | Do not ask users to retype repo or connection info between wizard steps |
| 3.3.8 Accessible Authentication (Minimum) | AA | No cognitive tests at login; allow paste into secret fields |
| 4.1.2 Name, Role, Value | A | Radix/shadcn primitives give this; custom cards need it too |
| 4.1.3 Status Messages | AA | Live regions for stream progress and "approval recorded", polite, not per token |

**Streaming-specific (inference).** Do not announce every token to screen readers. Announce state changes ("Run started", "Waiting for approval", "Run finished") through a polite live region and let users read the message when complete. Provide reduced-motion handling (2.3.3 is AAA, but respect `prefers-reduced-motion`).

---

# Storyboards

Legend: **[v0.1]** exists on origin/feat/approval-diff, **[new]** not built, **[v0.2]** from issue #91. Numbers refer to the annotations below each frame.

## S1. App shell, desktop [new; board route exists in v0.1]

```
+----------------------------------------------------------------------------+
| (1) Acme Corp v   > Board                (2) [ Search or jump (Ctrl+K) ]   |
|                                                       (3) bell(2)  (4) MA v|
+--------------------+-------------------------------------------------------+
| (5) NAVIGATION     |  (9) PAGE CONTENT                                     |
|  Board             |                                                       |
|  Waiting on you (3)|   (screen title, primary action at right)            |
|  Agents            |                                                       |
|  Connections       |                                                       |
|  ----------------  |                                                       |
|  Settings v        |                                                       |
|   MCP servers      |                                                       |
|   Org settings     |                                                       |
|   Audit            |                                                       |
|  ----------------  |                                                       |
| (6) Personal       |                                                       |
|  My agent          |                                                       |
|                    |                                                       |
| (7) Setup 2/5 [..] |                                                       |
| (8) Help           |                                                       |
+--------------------+-------------------------------------------------------+
```

1. Org switcher and breadcrumb. One org for most users. Breadcrumbs appear on nested pages (Board / Ticket 142).
2. Search plus command palette. Search is also reachable by a visible field, so it is not recall-based.
3. Notification bell: approvals and input needed first.
4. User menu: profile, notification settings, theme, sign out.
5. Text labels with icons. Items shown depend on role (section 8). Count badge on Waiting on you also has the number in text.
6. Personal org entry (ADR 0018), the same screens with their own data.
7. Setup checklist until the first PR (section 9). Dismissable.
8. Help in the same place on every page (WCAG 3.2.6).
9. Content area has one h1, primary action top right.

## S2. App shell, mobile [new]

```
+----------------------------+      +----------------------------+
| (1) Acme v   Board   (2)bell|      | (6) < Ticket 142      ...  |
+----------------------------+      +----------------------------+
| (3) [Todo][Doing][Waiting 3]|      | (title, status, agent)     |
|   [Done]   (segmented tabs) |      |                            |
|                            |      | (timeline ...)             |
| +------------------------+ |      |                            |
| | Fix failing test       | |      +----------------------------+
| | Test-fixer . Doing     | |      | (7) [ message...      ][>] |
| +------------------------+ |      +----------------------------+
| +------------------------+ |
| | Update deps  . Doing   | |
| +------------------------+ |
+----------------------------+
| (4) Board | Waiting(3) | Agents | More |
+----------------------------+
```

1. Org switcher collapses to the name. 2. Bell. 3. Columns become tabs; there is no drag, a "Move to..." action on the card (WCAG 2.5.7). 4. Bottom bar: four items, 44 px targets, text labels; More holds Connections, Settings, Personal, Help. 5. Detail pages hide the bottom bar. 6. Back arrow plus title. 7. Composer pinned above the keyboard.

## S3. Board [v0.1 columns; new filters and mobile tabs]

```
 Board                                     (1) [ + New ticket ]
 (2) [Mine v] [Agent v] [Status v]  (3) [x] Hide chat tickets

 +- Todo (4) ------+ +- Doing (2) -----+ +- Waiting on you (3) -+ +- Done -+
 |                 | |                 | | (4) ! APPROVAL       | |        |
 | Fix lint        | | Fix failing test| | Open PR in acme/api  | | Bump   |
 | Test-fixer      | | Test-fixer      | | [Review]  needs: you | | Go ver |
 |                 | | Running 3m  $.4 | |                      | |        |
 | Update docs     | | Update deps     | | ? INPUT NEEDED       | |        |
 | Docs-agent      | | Dep-agent       | | "Which branch?"      | |        |
 |                 | | Parked 12m      | | [Answer]             | |        |
 +-----------------+ +-----------------+ +----------------------+ +--------+
```

1. Primary action. 2. Filters; default to Mine for members, All for admins. 3. Hides auto-archived conversational tickets (ADR 0017 consequence). 4. Waiting on you shows approvals (admins and owners, exists in v0.1) and input-required clarifications (new). Each item has a text type label, not only an icon. Cards show agent, state in words, elapsed time and cost. Empty states say what to do ("No tickets yet. Create one or ask an agent").

## S4. Ticket and chat view with a streaming run [v0.1 timeline; new streaming and tool calls]

```
 Board / Ticket 142: Fix failing test            (1) [Mirrored: #eng] 
 Agent: Test-fixer (AI agent)  Owner: Platform team  Preset: Standard
 Status: RUNNING   Cost $0.42 of $5.00     (2) [ Stop run ]
+----------------------------------------------------------------------------+
| (3) Plan:  [x] Reproduce failure  [>] Fix cause  [ ] Run tests  [ ] Open PR |
+----------------------------------------------------------------------------+
|                                                                            |
|  Maria (14:02)                                                             |
|  The auth test fails on main. Can you fix it?                              |
|                                                                            |
|  Test-fixer, AI agent (14:02)                                              |
|  I will reproduce the failure first, then look at the token code.          |
|   (4) > Tool: run_tests  acme/api  failed, 1 test        [expand]          |
|   > Tool: read_file  auth/token.go  (212 lines)          [expand]          |
|  The token expiry is compared in local time. I am changing it to UTC...    |
|  (5) [ Show more ]  ...streaming text (cursor)                             |
|   > Tool: edit_file  auth/token.go  +3 -2                [expand diff]     |
|                                                                            |
|  (6) +-- APPROVAL REQUESTED ------------------------------------------+   |
|      | Open pull request in acme/api, 1 file (+3 -2)  [Review]         |   |
|      | Run is paused until you decide.                                  |   |
|      +------------------------------------------------------------------+   |
|                                                                            |
|  (7) [ 2 new events v ]                                                    |
+----------------------------------------------------------------------------+
| (8) [ Message Test-fixer...                         ] [Attach] [Send]       |
+----------------------------------------------------------------------------+
```

1. Slack/Teams mirror indicator and link [v0.2]. 2. Stop is visible while running; it states that work so far is kept. Parked runs replace it with "Waiting for you". 3. Live plan with state words. 4. Tool calls inline, collapsed, one line each with outcome; expandable to arguments and output (Amershi G11). 5. Streaming message with a height cap and "Show more". 6. Approval card embedded as a typed event; "Review" opens the full card (S5). 7. Jump pill when scrolled up; no forced auto-scroll. 8. Composer. `@` mentions other agents; their calls become child tickets (S8). Agent messages carry "AI agent" at all times. Screen reader: state changes announced, tokens not.

## S5. Approval card, desktop [v0.1 diff, risk flags, note, changed-state; new tier text, undo, viewed marks]

```
 Approval: Open pull request                         Status: WAITING
 Requested by Test-fixer (AI agent) on ticket 142  . Org Acme  . 14:09
+----------------------------------------------------------------------------+
| (1) What happens if you approve                                             |
|  Opens PR "Fix UTC expiry" in acme/api from fix/utc into main.            |
|  Tier: REVERSIBLE WRITE. Undo: close the PR.  Nothing is merged.           |
+----------------------------------------------------------------------------+
| (2) Check before approving                                                  |
|  ! Runs in CI:  .github/workflows/test.yml  CHANGED                        |
|  . Scripts: none   . Dependencies: none   . Binaries: none                |
|  . Size: 2 files, +14 -3   . Deletions: 0                                  |
+----------------------------------------------------------------------------+
| (3) Files        [x] auth/token.go    +3 -2   (viewed)                      |
|                  [ ] .github/workflows/test.yml +11 -1  (risk: CI) v        |
|     @@ -40,7 +40,10 @@                                                      |
|     -  if exp.Before(time.Now()) {                                          |
|     +  if exp.Before(time.Now().UTC()) {                                    |
| (4) Note (optional): [______________________________]                      |
| (5) [ Approve opening pull request ]   [ Deny ]   Cost so far $0.42        |
|     Version a3f9 . If the request changes you must review it again.        |
+----------------------------------------------------------------------------+
```

1. Plain consequence first, tier in text, undo stated before acting. 2. Risk facts, flagged ones first with "!" and the word WARNING in text (not colour alone). 3. Risky files pinned first, expanded; others collapsed; per-file viewed marks. 4. Optional note (exists). 5. Verb-and-object Approve and equal-weight Deny; request version shown; the 409 "changed" state replaces the buttons with "Reload and review again". Non-approvers see the card without buttons and with "Needs an admin".

## S6. Approval card, mobile full-screen sheet [new]

```
+----------------------------+
| (1) x Close      Approval  |
+----------------------------+
| Open pull request          |
| acme/api . Test-fixer (AI) |
|                            |
| (2) REVERSIBLE WRITE       |
| Undo: close the PR.        |
|                            |
| (3) ! CI config changed    |
|     2 files +14 -3         |
|                            |
| (4) Files (2)          >   |
|   auth/token.go  +3 -2     |
|   test.yml  +11 -1  !      |
|                            |
| Note (optional)  [ ... ]   |
+----------------------------+
| (5) [ Approve opening PR ] |
|     [ Deny ]               |
+----------------------------+
```

1. Close returns to the previous place without deciding. 2. Tier and undo in text. 3. Risk facts first. 4. Files open as a nested full-screen view with unified diff. 5. Sticky action bar, 44 px or taller targets, kept clear of the on-screen keyboard (WCAG 2.4.11). Reached by push or Slack deep link.

## S7. Agent wizard, every step [v0.1 single-page form with template, name, flavor, grants and repo picker; new: steps, builder start, review, draft resume]

```
STEP 1 of 4: Start                                       (1) [Quick create]
 (2) How do you want to begin?
  +--------------+ +--------------+ +--------------+ +---------------------+
  | Code fixer   | | Docs writer  | | Reviewer     | | Describe it to the  |
  | Fixes tests  | | Updates docs | | Reviews PRs  | | builder agent       |
  | Can open PRs | | Can open PRs | | Read only    | | (asks you questions)|
  +--------------+ +--------------+ +--------------+ +---------------------+
                                                         [ Next: Basics > ]

STEP 2 of 4: Basics                      (3) Draft saved at this step
  Name        [ Test-fixer         ]  hint: shown on every message
  Role        [ Fixes failing tests in our repos ]
  > Advanced (flavor: Hermes-compatible default)       (4)
                                                [ < Back ] [ Next: Access > ]

STEP 3 of 4: Access
  GitHub connection  [ acme-github v ]   (5) [ + Connect GitHub ]
  Repositories       [ acme/api x ] [ acme/web x ] [ pick... ]
  Preset             (o) Read only  ( ) Standard  ( ) Trusted         (6)
  Summary: "Can read code in 2 repos. Needs your approval to open PRs."
                                                [ < Back ] [ Next: Limits > ]

STEP 4 of 4: Limits and review
  Budget per day  [ $5 ]   Schedule [ none ]   (collapsed defaults)
  ---- Review ----------------------------------------------------------
  Template: Code fixer           [Change]
  Name: Test-fixer               [Change]
  Access: 2 repos, preset Standard  [Change]
  Budget: $5/day, no schedule    [Change]
  (7) This creates a DRAFT. An admin must activate it. You are an admin: 
      [ ] Activate after creating
                                        [ < Back ] [ Create draft ]

AFTER: (8) "Test-fixer created as a draft." [Activate] [Edit] [Create a ticket]
```

1. Experts skip steps via Quick create (the v0.1 form). 2. Templates first, each with what it can do and its highest tier. 3. A draft is created when step 2 completes (ADR 0020), so leaving is safe and the Agents list shows "Resume". 4. Advanced hidden by default. 5. Inline connection step returns here with state kept. 6. Preset radio with plain-language consequence; default is the safest that works for the template. 7. The review states the draft-versus-active rule and offers activation only to roles that can. 8. Next-step actions. Validation on blur for repo patterns; errors beside fields and in a summary.

## S8. Agent hierarchy view [new; list in v0.1 route /agents]

```
 Agents                      [List] [Structure]           [ + New agent ]
 (1) Filter: [ search...  ]  Team [All v]  Status [Any v]

 (2) Team: Platform                                          3 agents
  +-------------------------------------------------------------------+
  | Test-fixer     ACTIVE   Standard   Running 3m  $0.42/$5  [accepts |
  |                                                  personal agents]  |
  | Dep-agent      ACTIVE   Read only  Idle                            |
  | Release-bot    DRAFT    --         Needs admin activation          |
  +-------------------------------------------------------------------+
 Team: Finance                                               2 agents
  | Billing-analyst  ACTIVE  Read only  Idle   [shared by Org Beta]   |

 Structure tab (3):
  Acme Corp
  +- Platform
  |  +- Test-fixer
  |  +- Dep-agent
  +- Finance
  |  +- Billing-analyst
  Personal (Maria)
  +- Hermes  --calls-->  Billing-analyst (read tier)       (4)

 Delegation on a ticket (5):
  You > Hermes (personal) > Billing-analyst (Finance) > git-agent
        ticket 150          ticket 150.1               ticket 150.1.1
```

1. Filters and search first (scale answer). 2. List grouped by team, with status in words and badges (preset, budget, exposure). 3. Optional indented tree, keyboard operable; true containment only. 4. Delegation links shown as labelled edges only for the user's own personal agent (ADR 0019); read-tier cap stated. 5. The chain strip lives on each ticket; each hop is a link to the child ticket (ADR 0011); depth is platform-capped. Cost and approvals roll up to the parent. Cross-org agents carry a "provided by Org X, you pay" chip (ADR 0012).

## S9. Personal agent chat [v0.2]

```
+----------------------------------------------------------------------------+
| Hermes (AI agent, personal)   Owner: Maria   Memory: private   [Settings]   |
+----------------------------------------------------------------------------+
| (1) Today                                                                   |
|  You: What did the finance team spend on cloud last month?                  |
|  Hermes: Asking Billing-analyst (Finance, read only)...   (2) [view call]   |
|   > Called Billing-analyst  ticket 150.1  done  $0.03                       |
|  Hermes: $12.4k, up 8%. Source: Billing-analyst report 2026-09 (3) [1]      |
|                                                                            |
|  (4) Hermes wants to remember: "Maria prefers short summaries."            |
|      [Keep] [Don't keep]  [Manage memory]                                  |
|                                                                            |
|  (5) Billing-analyst asks for approval to export a CSV   -> [Review]       |
+----------------------------------------------------------------------------+
| [ Message Hermes...                                    ] [Send]  [Stop]     |
+----------------------------------------------------------------------------+
  Phone: this is the home tab. Composer above the keyboard, same cards.
```

1. A chat view is still a ticket (ADR 0017), with a "Chat" kind that auto-archives. 2. Every call to a team agent is visible and links to its child ticket; read-tier cap (ADR 0019) is shown. 3. Sources link to the report, file or memory entry. 4. Memory and persona changes surface as proposals the user can accept; the persona page shows version history and revert (persona-evolution.md). 5. Team approvals go to the team org's approvers (ADR 0019), so Maria sees "waiting on Finance approvers", not a Decide button.

## S10. Per-tier home screens

```
MEMBER (desktop)                          ORG ADMIN / OWNER
+-----------------------------+           +-----------------------------+
| Board (Mine)                |           | Waiting on you  (5)         |
| Waiting on you: 1 question  |           |  3 approvals, 1 draft to    |
| My tickets    [Doing][Done] |           |  activate, 1 question       |
| Agents (read-only list)     |           | Board (All)  Budgets: 62%   |
+-----------------------------+           | Setup checklist / alerts    |
                                          +-----------------------------+
PLATFORM ADMIN                            PERSONAL-ONLY USER
+-----------------------------+           +-----------------------------+
| Platform overview           |           | Chat with Hermes (v0.2)     |
|  Orgs 12  Failing runs 2    |           | Waiting on you: 0           |
|  Spend today  Policy alerts |           | Recent tickets              |
| Orgs table [filter]         |           | Settings (one item)         |
| Connector types, Auth types |           +-----------------------------+
| Banner: Acting as platform  |
|  admin when inside an org   |
+-----------------------------+
```

Same shell and components; only the nav items and home panels differ (section 8). Members see approvals they requested as read-only status.

## Flow storyboard: helm install to PR

```
F1 INSTALL (outside UI)         F2 LOGIN                         F3 CONNECT GITHUB
 $ helm install koinon ...      +--------------------+           Step 1/3 Create GitHub App
 Ready in ~8 min (target 15)    | Koinon             |            [guide, copy button]
 "Open https://koinon.local"    | [Sign in (OIDC)]   |           Step 2/3 Paste key (secret
 (1) health page: checks pass   | or First-run admin |             field, paste allowed)
                                +--------------------+           Step 3/3 Pick repos
                                (2) bootstrap admin              (3) returns to checklist
                                 banner: add OIDC

F4 CREATE AGENT                 F5 FIRST TICKET                  F6 RUN + APPROVAL
 Wizard 4 steps (S7)            Board empty state:               Ticket streams (S4)
 -> Draft -> [Activate]         "Try: Fix a failing test"        Plan, tool calls
 (4) checklist 3/5              [Create ticket]                  (6) Push/Slack: "Approve
                                (5) ticket opens on stream        opening PR in acme/api?"

F7 DECIDE                       F8 PR                            F9 AFTER
 Card (S5/S6): consequence,     Result card on ticket:           Checklist done. Prompt:
 risk, diff, Approve            "PR #88 opened. Undo: close"     "Review in GitHub" and
 (7) run resumes after approve  [Open in GitHub]                 one-question survey (HEART)
```

Frame notes: (1) the install health page names failing checks in plain words. (2) A bootstrap admin removes the IdP prerequisite from the 15-minute path (open question in ADR 0004). (3) State is saved when the user goes to GitHub and back. (4) The checklist is the progress indicator across the whole path. (5) The suggested ticket reduces blank-page cost. (6) The notification states the consequence and deep-links to the sheet. (7) The card shows the execution result after Approve (ADR 0042: Koinon executes the exact call). Funnel events for each frame feed the metrics in section 1.

---

# Decisions for procoduck

All 17 were decided on 2026-10-23; the answer follows each recommendation.

1. **Navigation model.** Recommend a left sidebar (desktop) plus bottom bar (phone), command palette as accelerator only. Alternative: top nav, which wastes the growing item count. Needed before the next UI. **Decided 2026-10-23: Left sidebar plus bottom bar, command palette as a shortcut.**
2. **Where chat lives in the nav.** Recommend: team users have no Chat item (chat is in the ticket); personal users get a Chat home in v0.2. Alternative: a global Chat item for everyone. Cost of the first: two homes; cost of the second: it contradicts ADR 0017. **Decided 2026-10-23: Superseded by [ADR 0055](../adr/0055-chat-as-its-own-object.md): chat is its own object; everyone gets it (v0.2). Personal orgs open on chat, team orgs on the board.**
3. **Define the member role.** Recommend: owner, admin, member (use agents, create tickets, see approvals read-only), with a v1 viewer role only if asked. Confirm what members can do with approvals; v0.1 gives decisions to admin and owner only. **Decided 2026-10-23: Owner, admin, member. Members see approvals read-only ([ADR 0004](../adr/0004-human-identity.md) amendment).**
4. **Conversational tickets.** Recommend a "Chat" ticket kind, hidden from the board by default, auto-archived after idle, with Promote to task. Closes ADR 0017's open question. **Decided 2026-10-23: Superseded by [ADR 0055](../adr/0055-chat-as-its-own-object.md): a chat is not a ticket kind; it becomes a ticket when the person or the agent proposes it and the person confirms.**
5. **Agent identity.** Recommend a fixed "AI agent" label and a neutral glyph on all agents, no human avatars or typing theatre, persona limited to a versioned "style and notes" page separate from the read-only Core. Matches persona-evolution.md. **Decided 2026-10-23: Agents carry a neutral "AI agent" label.**
6. **Attribution wording.** Recommend "Hermes, on behalf of Maria" everywhere an agent acts for a person, and a chain strip for delegation. Decide the exact phrase once. **Decided 2026-10-23: Attribution reads "Hermes (for Maria)".**
7. **Approval card standard.** Recommend the S5 layout: consequence and tier in text, risk facts, diff with viewed marks, undo statement, equal buttons, request version, changed-state re-review. Keep batching off for `external-or-irreversible`. **Decided 2026-10-23: The S5 approval card is the standard.**
8. **Batch approvals.** Recommend: not in v0.1; in v1.0 only for same-agent, same-tier, non-irreversible items with a summary table. Measure decision time before shipping it. **Decided 2026-10-23: No batch approvals until v1, and then narrow.**
9. **Approval timeouts.** Recommend: no auto-approve or auto-deny; runs stay parked, cost stops, reminders after a set delay. Needs a platform default and an escalation path. **Decided 2026-10-23: An undecided approval parks, reminds after 24 h, then escalates.**
10. **Wizard versus form.** Recommend the four-step wizard with Quick create for repeat use, a draft created at step 2, and a mandatory review step. Keep the v0.1 form as Quick create. **Decided 2026-10-23: Wizard plus Quick create.**
11. **Mobile scope.** Recommend phone-complete for: Waiting on you, approval sheet, ticket thread and reply, stop run, personal chat. Everything else must reflow at 320 px but need not be optimised. **Decided 2026-10-23: Desktop only in v0.1.**
12. **Notifications.** Recommend defaults of approvals and input needed only, a digest option, and Slack/Teams mirroring as the main channel in v0.2. Decide whether to ship PWA push in v0.1 (extra work, iOS caveats). **Decided 2026-10-23: In-app and email notifications; no push.**
13. **Hierarchy visual.** Recommend grouped list with badges as the default, an indented tree as a secondary tab, a per-ticket delegation strip, and no global graph in v1. **Decided 2026-10-23: Grouped list with a tree tab.**
14. **Platform admin placement.** Recommend a separate area in the same shell with a persistent "acting as platform admin" banner inside orgs. **Decided 2026-10-23: Platform admin gets its own area with a banner.**
15. **Measurement.** Recommend local, in-instance event logging (approval card shown, expanded, decided, outcome; funnel events), an org-admin dashboard, SUS after test sessions, and no external telemetry by default. Decide whether to offer an opt-in anonymous report. **Decided 2026-10-23: Metrics stay local; no opt-in report.**
16. **Accessibility target.** Recommend WCAG 2.2 AA as an explicit acceptance criterion, with an automated check in Playwright (axe) and a manual keyboard and screen-reader pass for the approval card and wizard. Decide before v0.1 ships. **Decided 2026-10-23: WCAG 2.2 AA, with axe in CI.**
17. **Setup checklist.** Recommend a home-screen checklist until first PR, and a bootstrap admin so the first run does not need OIDC (links to ADR 0004's open question). **Decided 2026-10-23: A setup checklist shows until the first PR.**

# Sources

Product and project sources (read directly):
- Koinon ADRs 0004, 0008, 0011, 0012, 0017, 0018, 0019, 0020, 0022 and 0042 on origin/docs/plan-v0.1 (`docs/adr/`).
- `docs/research/persona-evolution.md` on origin/docs/research-persona.
- `web/` on origin/feat/approval-diff (package.json, router, board, approval card, agent wizard, risk helpers).
- GitHub issue #91, "v0.2 design: persistent coworkers and the personal agent".

External sources were checked on 2026-10-22 (existence, metadata and attributed claims where abstracts were available); items marked unverified were not confirmed.
- Nielsen Norman Group: 10 Usability Heuristics (nngroup.com/articles/ten-usability-heuristics/); The Need for Web Design Standards (https://www.nngroup.com/articles/the-need-for-web-design-standards/); Information Scent (nngroup.com/articles/information-scent/); Response Times: The 3 Important Limits (nngroup.com/articles/response-times-3-important-limits/); Why You Only Need to Test with 5 Users (nngroup.com/articles/why-you-only-need-to-test-with-5-users/); Wizards: Definition and Design Recommendations (nngroup.com/articles/wizards/); Progressive Disclosure (nngroup.com/articles/progressive-disclosure/); Breadcrumbs: 11 Design Guidelines (nngroup.com/articles/breadcrumbs/); Modal and Nonmodal Dialogs (nngroup.com/articles/modal-nonmodal-dialog/); Mugunthan, Overcoming the Articulation Barrier in Generative AI Using Hybrid Interfaces, 13 Aug 2023 (https://www.nngroup.com/articles/ai-articulation-barrier/); Onboarding Tutorials vs. Contextual Help (nngroup.com/articles/onboarding-tutorials/).
- Baymard Institute: checkout usability research (baymard.com/blog). Used only for direction on form length.
- W3C: WCAG 2.2 (w3.org/TR/WCAG22/) including SC 1.4.1, 1.4.3, 1.4.10, 1.4.11, 2.4.11, 2.5.7, 2.5.8, 3.3.7, 3.3.8, 4.1.3; WAI-ARIA Authoring Practices, Tree View pattern.
- Apple Human Interface Guidelines (developer.apple.com/design/human-interface-guidelines/): tab bars, layout, touch target size. Material Design 3 (m3.material.io): navigation bar, window size classes, touch target size.
- GOV.UK Design System (design-system.service.gov.uk): Question pages (one thing per page), Check answers, Task list, Error message, Error summary.
- Rodden, Hutchinson, Fu. Measuring the User Experience on a Large Scale: User-Centered Metrics for Web Applications. CHI 2010.
- Brooke. SUS: A "Quick and Dirty" Usability Scale. 1996.
- Amershi et al. Guidelines for Human-AI Interaction. CHI 2019.
- Google PAIR. People + AI Guidebook (https://pair.withgoogle.com/guidebook; chapter names unverified).
- Bansal et al. Updates in Human-AI Teams. AAAI 2019 (I cited it as CHI 2019 above: this is a correction, it is AAAI 2019); Bansal et al. Does the Whole Exceed its Parts? CHI 2021.
- Buçinca, Malaya, Gajos. To Trust or to Think: Cognitive Forcing Functions Can Reduce Overreliance on AI. CSCW 2021, PACM HCI 5(CSCW1) art. 188, doi 10.1145/3449287.
- Lee and See. Trust in Automation: Designing for Appropriate Reliance. Human Factors, 2004. Parasuraman and Riley. Humans and Automation: Use, Misuse, Disuse, Abuse. 1997. Parasuraman, Sheridan, Wickens. A Model for Types and Levels of Human Interaction with Automation. IEEE SMC-A, 2000. Bainbridge. Ironies of Automation. Automatica, 1983.
- Egelman, Cranor, Hong. You've Been Warned. CHI 2008. Akhawe and Felt. Alice in Warningland. USENIX Security 2013. Anderson et al. How polymorphic warnings reduce habituation in the brain. CHI 2015, pp. 2883-2892. Ancker et al. Effects of workload, work complexity, and repeated alerts on alert fatigue. BMC Med Inform Decis Mak 17:36, 2017, doi 10.1186/s12911-017-0430-8.
- Gray et al. The Dark (Patterns) Side of UX Design. CHI 2018. Mathur, Acar, Friedman, Lucherini, Mayer, Chetty, Narayanan. Dark Patterns at Scale. CSCW 2019, doi 10.1145/3359183. EU Digital Services Act, Art. 25. EU AI Act, Art. 50.
- Iqbal and Horvitz. Disruption and Recovery of Computing Tasks. CHI 2007. Mark, Gudith, Klocke. The Cost of Interrupted Work. CHI 2008.
- Ghoniem, Fekete, Castagliola. On the readability of graphs using node-link and matrix-based representations. Information Visualization, 2005. Shneiderman. Tree visualization with tree-maps. ACM TOG, 1992.
- Shanahan, McDonell, Reynolds. Role play with large language models. Nature, 2023. Subramonyam et al. Bridging the Gulf of Envisioning. CHI 2024, doi 10.1145/3613904.3642754. Feng, McDonald, Zhang. Levels of Autonomy for AI Agents. arXiv 2506.12469, 2025.
- First-party product docs (patterns only): GitHub (reviewing pull requests, Projects, Copilot coding agent), Linear (docs on inbox, triage, command menu), Slack (help centre on threads, Activity, notifications), ChatGPT and OpenAI agent documentation, Claude.ai and Claude Code documentation, Cursor documentation, Devin documentation.

# Evidence gaps

1. **External sources were checked on 2026-10-22 (existence, metadata and attributed claims where abstracts were available); items marked unverified were not confirmed.** One known correction: Bansal et al., Updates in Human-AI Teams, was AAAI 2019, not CHI.
2. **Paperclip.** I could not verify a first-party description of its UI, so no claims are made about it. Add after reading its docs.
3. **First-party product behaviour** (GitHub, Linear, Slack, ChatGPT, Claude.ai, Cursor, Copilot, Devin) is described from memory of their docs and may have changed. Mark **reported**; re-check before copying any pattern.
4. **Onboarding drop-off.** No high-trust quantitative source for developer-tool onboarding found. The drop-off table is **inference**. Koinon's own funnel (section 1) is the real evidence.
5. **Rubber-stamping in agent approval specifically.** Evidence is from security warnings, clinical alerts and automation bias. I found no controlled study of approval prompts for coding agents. The "under 3 s with the diff unexpanded" threshold is a guess to be tuned.
6. **"AI" sparkle overuse.** No primary source. Treated as an inference from general heuristics.
7. **Hierarchy visualisation at agent scale.** The graph-readability evidence is general; none concerns agent hierarchies. Revisit when real orgs exist.
8. **Mobile approval usage.** No data on how often approvals will happen on phones in Koinon. The recommendation rests on the assumption that approvals block runs and users are away from desks.
9. **Roles.** The member role and the exact approver rules for group approvals (ADR 0017 open question) are not specified in the ADRs I read.
10. **v0.2 persona UI.** The persona-evolution evidence is mostly adjacent (not about self-editing personas over weeks), so the persona history and revert UI is **inference**.
11. **Accessibility of streaming UIs.** WCAG gives general rules (4.1.3 status messages); I found no tested guidance for streaming agent output with screen readers. Plan a manual test.