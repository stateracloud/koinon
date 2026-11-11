# Koinon

Koinon is an org-and-work layer for AI agents on Kubernetes: each org sets up its own agents,
connections, providers and budgets, agents take work as tasks and run in isolated sandboxes, and the
inbox shows people what needs them. Sandboxing, isolation and credential brokering are reused from
NVIDIA OpenShell and agentgateway rather than built.

## Language

### Tenancy and people

**Org**:
The unit of tenancy: a team's or a person's space that owns agents, connections, providers, budgets
and members. Everything a user configures belongs to exactly one org.
_Avoid_: Organization, tenant, team, workspace, namespace

**Personal org**:
The org every user gets automatically and alone, where their own agents live and their login events are recorded.
_Avoid_: User org, private org, home org

**Role**:
A person's standing in one org: viewer, member, admin or owner, each with the rights of the ones
below. Members use agents and answer questions, while admins and owners configure the org and decide
approvals; every org keeps at least one owner assigned by hand.
_Avoid_: Permission level, group, access level

**Org admin**:
Anyone with the admin or owner role in an org. "The admin role" is the narrow term that excludes owners.
_Avoid_: Admin (unqualified), administrator

**Platform**:
The level above every org in one install, whose settings, defaults and limits apply to all orgs.
_Avoid_: Global, cluster, system, instance

**Platform admin**:
A person who runs the platform: sets platform ceilings and defaults, curates what every org sees and allows internal hosts. It is not an org role.
_Avoid_: Superuser, cluster admin, global admin, org owner

### Agents and harnesses

**Agent**:
A persistent, named worker in an org with its own instructions, grants, triggers, model, budget and
home. It works on one task at a time, in its own sandbox.
_Avoid_: Bot, assistant, worker, agent instance

**Agent version**:
One numbered, approved configuration of an agent: instructions, grants, triggers, model, run limits
and question timings. Changes take effect only when a newer version is activated.
_Avoid_: Agent config, revision, spec, agent definition

**Activate**:
An admin or owner approving an agent version, with its grants and triggers, so that new runs use it.
_Avoid_: Deploy, publish, enable, approve (an agent)

**Archived agent**:
An agent retired from use: it takes no work and cannot be resumed.
_Avoid_: Deleted agent, disabled agent, archived sandbox

**Template**:
A curated starting point for a new agent: flavor, instructions, suggested grants, triggers and
default limits. An agent made from it keeps its own copy and is not changed by later edits to it.
_Avoid_: Blueprint, preset, agent type, recipe

**Harness**:
The agent program that runs in a sandbox and talks to the model, such as OpenCode, driven by Koinon. An agent selects one.
_Avoid_: Agent runtime, CLI, framework, flavor (for the program itself)

**Harness definition**:
Koinon's record of how to obtain and run one harness. It is built-in (shipped in the sandbox image) or custom (registered on an install), and experimental or verified.
_Avoid_: Flavor, adapter, plugin, harness image

**Flavor**:
The earlier word for a harness definition: Koinon's tested packaging of one harness as a sandbox image. It is being retired; templates, API and database still say `flavor` until the rename lands.
_Avoid_: Agent image, harness image, runtime; flavor for anything new

**Ephemeral agent**:
An agent whose every task gets a fresh sandbox without a home, deleted when the task ends. Agents are persistent by default.
_Avoid_: Stateless agent, throwaway agent, one-shot agent

**Unhealthy agent**:
An agent that takes no runs until an admin resumes it, because its sandbox kept getting stuck, kept failing or never became ready.
_Avoid_: Broken agent, paused agent, disabled agent

**Agent notes**:
The short note an agent writes at the end of each turn on where its task stands and what comes next. It is shown on the task and is never a reply.
_Avoid_: Summary, status update, memo

### Work: tasks, triggers, runs

**Task**:
Koinon's record of one piece of work, owned by one agent: its messages, runs, approvals,
questions and cost. A person, a trigger or another agent creates it, and nobody moves it along by hand.
_Avoid_: Ticket, job, issue, card, request

**Task state**:
Where a task stands: submitted, working, input required, completed, canceled or failed.
_Avoid_: Column, status, To do / In progress / Waiting on you / Done

**Timeline**:
The ordered record of everything on a task: messages, run events, approvals, questions, notes and state changes.
_Avoid_: History, activity feed, log, event stream

**Inbox**:
The home screen: the list "Waiting on you" of items that need the viewer, next to the open tasks and direct-message threads.
_Avoid_: Dashboard, home page, notification center

**Inbox item**:
One thing waiting on a person: an approval to decide, a question to answer, a task waiting for their reply, or an unhealthy agent.
_Avoid_: Notification, alert, card, to-do

**Board**:
A secondary, read-only view of an org's tasks grouped by task state.
_Avoid_: Kanban, queue, backlog

**Trigger**:
A rule on an agent version that starts a task without a person: a type, a filter and an instruction that becomes the task's first message.
_Avoid_: Hook, automation, subscription, watch, rule

**Schedule trigger**:
A trigger that fires at the times of a cron expression in a named time zone.
_Avoid_: Cron job, timer, scheduled task

**Direct-message trigger**:
A trigger that lets an org's members message an agent from the inbox.
_Avoid_: DM channel, chat trigger, message trigger

**Direct-message thread**:
The messages between people and an agent that one direct message starts; the thread is one task. "Chat" is reserved for a planned, separate object.
_Avoid_: Chat, conversation, DM

**Channel event trigger**:
A planned trigger that starts or wakes a task from a Slack or Teams channel message or mention.
_Avoid_: Chat event, message trigger

**Notify target**:
Who a trigger's tasks ask when the agent names nobody: the org's admins by default, or its owners,
all members, one person or, later, a chat channel. The UI calls it "Who is asked".
_Avoid_: Assignee, escalation contact, on-call

**Run**:
One execution of an agent on a task, from queued to succeeded, failed or canceled. A run can park and resume many times.
_Avoid_: Job, execution, invocation, session

**Turn**:
One prompt to the harness and its response, from the start or a resume until the harness ends it. A run has one or more turns.
_Avoid_: Step, iteration, round, message

**Session**:
The harness's conversation memory for one task, kept in the agent's home and reloaded by each run of the task.
_Avoid_: Run, context, thread, conversation, trace session

**Fresh session**:
A new session for an open task, given a handoff of the task's title, the agent's last notes and recent replies,
used when a new version applies to open tasks or the home was lost.
_Avoid_: Session reset, restart, new conversation

**Park**:
A run stepping aside, holding no compute, while it waits for an approval, a question's answer or
another agent's reply; it resumes exactly where it stopped.
_Avoid_: Suspend, pause, block, sleep, hold

**Failure class**:
The kind of failure a failed run carries, such as transient, throttled, context overflow, budget, policy, fatal or sandbox unavailable. It decides retries and what people are told.
_Avoid_: Error type, error code, failure reason

### Isolation and sandboxes

**Sandbox**:
The isolated environment, one per agent, where the harness runs. It reaches the outside only through
OpenShell's policy enforcement and the gateway, and never holds a real credential.
_Avoid_: Container, pod, VM, box, environment

**Isolation tier**:
How strongly an install isolates its sandboxes, the same for every org: vm (a VM per sandbox, the
golden path), hardened (a shared kernel with every available layer) or dev (kind and CI only).
_Avoid_: Isolation level, security level, tier (unqualified), sandbox mode

**Home**:
The part of an agent's sandbox that keeps its work between runs: sessions, notes, memory and scratch work.
_Avoid_: Workspace, volume, disk, home directory (for the whole)

**Home checkpoint**:
A saved copy of an agent's home in object storage, restored into a new sandbox so the agent keeps its session.
_Avoid_: Backup, snapshot, image

**Archived sandbox**:
A sandbox deleted after a long idle stop while its home lives on in a checkpoint; the next run restores it into a fresh sandbox.
_Avoid_: Archived agent, hibernated sandbox, cold sandbox

**Warm window**:
How long a parked run's sandbox stays running before it stops: 15 minutes when waiting on a person or another agent.
_Avoid_: Park-warm time, keep-alive, idle timeout

**Stuck sandbox**:
A sandbox with a process that cannot be ended after a turn. Koinon saves its uncommitted work and recreates it.
_Avoid_: Unkillable sandbox, hung sandbox, zombie sandbox

**Saved work**:
Uncommitted changes Koinon rescues from a sandbox before recreating it, pushed to a new branch only on approval.
_Avoid_: WIP, stash, backup

**Sandbox observer**:
Koinon's read-only check that explains why a sandbox never became ready, such as an image that cannot be pulled or a crash loop.
_Avoid_: Watcher, prober, diagnostics

**OpenShell**:
NVIDIA's sandbox runtime that Koinon reuses for sandbox lifecycle, isolation, network policy and credential placeholders.
Its per-sandbox supervisor is the only way out of a sandbox.
_Avoid_: Sandbox engine, executor

### Credentials and gateway

**Gateway**:
agentgateway, the one path from sandboxes to models and MCP servers: it holds the real keys, enforces spend limits and routes tool calls.
_Avoid_: Proxy, LLM router, model gateway, MCP gateway, OpenShell gateway

**Gateway mode**:
How the gateway is run for an install: standalone, kubernetes or shared.
_Avoid_: Deployment mode, gateway type

**Gateway driver**:
The Koinon code that runs one gateway product behind Koinon's gateway interfaces: agentgateway's today, Koinon's own from v0.2 (ADR 0097, proposed). Nothing outside a driver names the product.
_Avoid_: Gateway adapter, backend, plugin

**Gateway behaviour suite**:
The tests of what a running gateway does on the request path (key refusals, header handling, usage per format, which MCP messages the guard sees, budget stops), run against the real image. A gateway version or driver is accepted only when it passes (ADR 0097).
_Avoid_: Conformance tests, gateway e2e, contract tests

**Gateway key**:
The key an agent's sandbox uses to call the gateway. It carries the agent's spend limits and is worthless outside the gateway.
_Avoid_: Virtual key, agent API key, model key

**Placeholder**:
A stand-in credential value inside a sandbox, swapped for the real secret only on requests to allowed endpoints.
_Avoid_: Fake key, dummy token, masked secret

**Provider record**:
OpenShell's stored credential for a sandbox, injected only for the endpoints a grant allows.
_Avoid_: Provider (unqualified), credential provider

### Models

**Provider**:
An org's configured source of models: an endpoint of a provider type such as OpenRouter, Anthropic,
Ollama or an OpenAI-compatible API, usually with an API key.
_Avoid_: Vendor, backend, inference provider, LLM provider

**Model**:
A model name served by one of the org's providers, with its API format. Agents ask for it by its plain name.
_Avoid_: LLM, engine, deployment

**API format**:
The wire format a model speaks, its family's native one: openai or anthropic.
_Avoid_: Protocol, dialect, schema

### Cost and budgets

**Price**:
US dollars per million tokens for each kind of token, for the models whose name matches a pattern, with optional price tiers for long inputs. An org's own price wins over the platform price list, which wins over the default price list.
_Avoid_: Rate, rate card, tariff

**Default price list**:
The prices shipped in the Koinon binary, checked by hand against each provider's own page; it changes only with a release.
_Avoid_: Catalog, built-in prices, shipped defaults

**Platform price list**:
The install-wide, versioned prices a platform admin accepts (or the bounded auto-apply writes) between org prices and the default price list; an entry retires once a release ships the same price.
_Avoid_: Install overlay (for prices), global prices, price override

**Price proposal**:
A difference between a public price source and the price Koinon uses for a model, waiting to be accepted, dismissed or auto-applied, with its source, commit, hash and fetch time.
_Avoid_: Price suggestion, price update, sync

**Cost**:
What a model request, run or task cost in US dollars, as the spend ledger records it: the provider's billed cost where the provider reports one per request (an OpenRouter request, once settled), otherwise worked out by Koinon from the tokens reported and the price valid at the time (Anthropic and OpenAI direct, whose days are compared with the provider's cost API by the cost reconciliation).
_Avoid_: Bill, usage, charge (for a total)

**Cost reconciliation**:
The daily comparison of the spend ledger's cost of one direct provider key with the cost the provider's own cost API reports for that key's workspace, project or API key, recorded per UTC day as ok, out of tolerance or error. It never changes an entry.
_Avoid_: Billing sync, cost import

**Spend**:
The sum of costs counted against a budget in its window: the UTC day for an agent, the calendar month for an org.
_Avoid_: Usage, consumption, burn

**Spend ledger**:
Koinon's authoritative record of every model request and its cost, from which budgets, run limits and reports are computed.
_Avoid_: Billing log, usage log, cost table

**Usage record**:
One gateway request as the spend ledger reads it: request, key, agent and model, status, and the token counts per kind. Every gateway driver yields them; the ledger reads no gateway's own log fields (ADR 0097).
_Avoid_: Access log entry, usage event, log record

**Call reservation**:
The worst-case cost of a model call (its input plus its `max_tokens` of output) held against every budget level before the gateway forwards the call, and settled to the real cost when it ends (ADR 0097, from v0.3).
_Avoid_: Pre-charge, hold, reserve (that is a provisional entry's `max_tokens` bound)

**Budget**:
A spend limit on one scope and window: an agent's daily limit, an org's monthly cap or the platform
ceiling. Reaching one stops (cancels) every open run in its scope; it never parks them.
_Avoid_: Quota, allowance, spend cap (generic)

**Daily limit**:
An agent's budget per UTC day, starting from its template's spend limit.
_Avoid_: Agent budget, daily budget, daily cap

**Monthly cap**:
An org's budget per calendar month in UTC.
_Avoid_: Org budget, monthly limit, monthly budget

**Platform ceiling**:
The highest monthly cap a platform admin allows one org.
_Avoid_: Budget ceiling, org ceiling, hard cap, content ceiling

**Run limits**:
The three limits on one run, model requests, spend and time (wall clock, parked time excluded); the first one reached ends the run. Tokens are reported, not limited.
_Avoid_: Token cap, max tokens, run cap, run budget

**Notification**:
An in-app message to a person outside the inbox list, such as a budget reaching 80% or 100%, an unhealthy agent or a question reminder.
_Avoid_: Notice, alert, inbox item, email

**Wrap-up notice**:
Koinon's message to an agent as a run nears one of its limits, asking it to finish and report; the only "notice" in Koinon.
_Avoid_: Timeout warning, notification

### Approvals and questions

**Grant**:
An agent's access to one connection: a subset of its scopes plus a preset. Grants belong to the agent version and take effect when it is activated.
_Avoid_: Permission, binding, access rule, role binding

**Scope**:
One capability a connector offers, such as reading a repository or opening a pull request, each with a risk tier.
_Avoid_: Permission, capability, right

**Risk tier**:
How dangerous a scope is: read, reversible write, external or irreversible, or forbidden.
_Avoid_: Risk level, severity, tier (unqualified)

**Decision**:
Whether a call may run: allow, ask (it needs an approval) or deny. A grant's preset sets it per risk tier; each MCP tool has its own.
_Avoid_: Auto, never, tool tier, mode

**Preset**:
A grant's mapping of each risk tier to a decision: Read only, Standard or Trusted.
_Avoid_: Policy, profile, mode, approval level

**Ask tier**:
The calls whose decision is ask. The agent's call returns at once, the run parks, and after approval Koinon itself makes the exact call.
_Avoid_: Approval mode, human-in-the-loop, confirm tier

**Approval**:
A person's decision on one exact call in the ask tier, bound to its arguments; the call is made only if it is approved.
_Avoid_: Ask, confirmation, review, permission request, question

**Scoped approval**:
An approval that also covers later matching calls, same agent, connection, tool and target, for one task or for the agent until a date.
_Avoid_: Standing approval, approval rule, blanket approval, auto-approval

**Question**:
What an agent asks a person instead of guessing: text, optional choices and a draft, and a required safe default used if it is dismissed or expires. The run parks until the first answer.
_Avoid_: Ask, prompt, input request, approval

### Connectors and MCP

**Connector**:
A platform-curated kind of external system Koinon can reach, such as GitHub, Gitea or Slack, with its auth type, scopes and setup guide.
_Avoid_: Integration, plugin, app, provider

**Connection**:
An org's configured instance of a connector, such as one GitHub App installation, with its credentials, base URL and status.
_Avoid_: Integration, account, connector (for the instance), card

**Code host**:
A connector for a Git hosting system: GitHub, GitHub Enterprise Server or Gitea in v0.1.
_Avoid_: Forge, SCM, VCS provider, Git provider

**MCP server**:
A remote tool server an org connects. Agents reach its tools only through the gateway, which adds the server's credential.
_Avoid_: Tool server, plugin, server (unqualified)

**MCP sign-in**:
Koinon's stored OAuth authorization to one MCP server, owned by an org or a user and made once in the browser; the gateway adds its token to each call.
_Avoid_: OAuth grant, grant, token, OAuth connection

**Koinon tools**:
The tools Koinon itself offers every active agent without a grant, such as asking a person a question or canceling its own task.
_Avoid_: Built-in tools, system tools

### Observability

**Audit log**:
An org's tamper-evident record of every configuration change and every decision that matters, written with the change itself.
_Avoid_: Activity log, event log, history

**Audit batch**:
The audit events one transaction collects before they are written: callers add events to it while they make the change, and the audit module writes them just before commit, the install events first and then the org events by org ID, so an event commits with its change or not at all and no caller chooses the order (ADR 0100 decision 1.7).
_Avoid_: Audit queue, audit buffer, event list

**Install audit chain**:
The install-wide audit log of platform actions that belong to no org, such as platform price changes, written only by koinon-api (ADR 0100).
_Avoid_: Platform log, global audit log

**Install mark**:
The random token in the Secret `koinon-install-mark`, made once per deployment and not stored in Postgres. A hash of it labels every OpenShell object the install makes, and an install refuses objects with another mark, so a restore next to the live install cannot touch them (ADR 0106). The same hash also stamps API tokens and sessions (ADR 0110).
_Avoid_: Install ID (that one is in the database), namespace hash

**Install binding**:
The one `install_binding` row in Postgres that records which install owns the database, by the hash of its install mark. Each process computes a state against it: `same` (this install owns it), `copy` (another install does) or `unknown` (no row). A `copy` refuses API tokens and sessions that another install issued, until a platform admin runs adopt-restore or rebinds a token (ADR 0110).
_Avoid_: Install lock, database owner

**System actor**:
The actor of an audit event that Koinon itself caused, named `<component>:<function>`, such as `koinon-api:price-refresh`; the format is frozen and an ID is never renamed (ADR 0100 decision 2).
_Avoid_: Bot, auto, system user

**Run trace**:
The spans of a run's turns, tool calls, waits and model calls, exported to a tracing backend an org or the platform chose.
A backend groups them by run into a trace session, which is not the harness session.
_Avoid_: Telemetry, logs, transcript

**Content level**:
How much prompt and response content a run trace carries: none, redacted or full.
_Avoid_: Verbosity, detail level, redaction level

### Platform operations

**Install**:
One deployment of Koinon on a cluster, with its orgs, isolation tier, gateway mode and platform settings.
_Avoid_: Instance, cluster, tenant, environment

**Control plane**:
Koinon's own services, which hold the orgs and their work and drive the sandboxes; no agent code runs in them.
_Avoid_: Backend, server, operator, manager

### Contributing

**Claim**:
An exclusive hold on one issue by one lane of one human (the ref `claim/<N>`, ADR 0104), so two agents never work the same issue; it lasts until its pull request merges, or it is released.
_Avoid_: Lock, assignment, reservation

**Ready queue**:
The issues people have marked Ready, ordered by priority and then age, from which agents take work.
_Avoid_: Backlog, to-do list
