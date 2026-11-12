# 0081. Run limits: model requests, spend and time; tokens are reported, not enforced

- Status: Accepted
- Date: 2026-10-29
- Area: work-model
- Tags: runs, budgets, spend-ledger, pricing
- Amends: [0041](0041-koinon-spend-ledger-and-budget-stops.md) (the run level of the net key limit is built), [0050](0050-spend-ledger-ingestion-and-net-limits.md) (decision 5, the token cap, is replaced), [0049](0049-v01-scope-decisions.md) (the per-run caps)
- Relates to: [0066](0066-triggers-and-scheduling.md) (the wrap-up notice of old issue 183), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0068](0068-prompt-caching-always-on.md), [0023](0023-observability-and-audit.md)
- Issue: old issue 243 (decision by procoduck, 2026-10-29); the per-task spend cap of old issue 183 comes after v0.1.0

## Context

A run was capped by `limits.run.maxTokens`: the sum of `input + output` tokens of its ledger
entries, checked by `scheduler.RecordTokenUsage` (ADR 0050, decision 5). The gateway's convention
puts cache reads and writes inside the input count, so the cap measured roughly *context size times
the number of requests*. On the rc.5 home-lab run two researcher tasks failed at 2.8M tokens after 29
and 33 requests, six minutes in, with a 1M cap. A cache-heavy loop is cheap: with prompt caching on
(ADR 0068, in effect since rc.5) the same requests cost a few cents. The cap stopped the cheapest
runs and said nothing about cost or about loops (old issue 243).

The research (session scratchpad, `research/run-cap.md`, 2026-10-29) found that no provider bills
"tokens"; every one bills token types at different rates. Tokens differ in size between models
(Claude counts about 30% more tokens for the same text from 4.7 on). The universal run guard in
agent products is a count of steps, and the money guard, where there is one, is in USD. Token caps
per run are rare, and the products that have one, such as Claude Code and Codex, manage context
with compaction instead of a stop.

## Decision

1. **Three limits end a run; the first one reached wins.**
   - `limits.run.maxModelRequests`: the loop guard. It counts **every** model request through the
     agent's key, the harness's own title, summary and compaction calls included. It is exact (one
     per ledger entry, estimated and failed requests included), the same on every model, and still
     binds on a model priced at $0.
   - `limits.run.maxSpendUSD`: the money guard, in the unit of the daily and monthly budgets. It is
     the ledger's settled cost: Koinon's own price at the request's time, caching, thinking,
     estimates and the provider's settlement already handled (ADR 0056).
   - `limits.run.maxDuration`: the wall clock, unchanged. Parked time does not count.
2. **A run is canceled when it has used its limit.** At `model_requests >= maxModelRequests`, the
   Nth request has been made and the run ends; at `spend >= maxSpendUSD` the same. The run's
   cancel carries reason `request_cap` or `run_spend_cap` (`wall_clock_cap` for time), failure class
   `budget` for the first two, and the amounts in its event. The failure class stays in the existing
   fixed set (ADR 0023); no label is added to any metric by this decision except the `limit` of the
   wrap-up counter (`time`, `requests`, `spend`).
3. **The spend limit is the "run" level of the net key limit** (ADR 0041, decision 5, which
   named it and left it unbuilt). While an agent has a starting or running run with a spend limit,
   the key's pushed limit is `min(run, agent daily, org monthly, platform ceiling)` remaining, net of
   the spend recorded in each window and, for the agent, org and platform levels, the reserves of
   provisional entries (the run level holds none back: see the amendment below). In standalone mode
   agentgateway therefore refuses the key's next request between ledger passes; the overshoot is
   bounded by one in-flight response per level, as for the others. A run start and a run end change
   the pushed limit once each (one config reload in standalone mode). In Kubernetes mode there is no
   gateway budget and the ledger alone enforces it, as for every level. The request limit is not a
   key level: agentgateway has no request count per key, and the ledger's count is exact.
4. **One generic spend-scope check.** `scheduler.CheckRunLimits` reads the run's usage from the
   ledger and walks a list of *spend scopes*, each a budget level with the spend inside it
   (`budget.RunLevel` is the only one). The per-task cap of old issue 183 is another entry of that list: a
   level named for the task that sums the task's entries, with a nullable limit column on the task.
   The check, the cancel, the net key limit and the reconciler's stop need no change. It is not
   built here, and nothing in the run limits assumes it never will be.
5. **When the check runs.** After a ledger entry is recorded (after the agent, org and platform
   budgets are checked, so the broadest limit reached names the stop, and a request that reaches
   both the agent's daily limit and the run's limit reports the daily limit), and after a provider
   settlement changes an entry (a settlement can raise a cost). It reads the entries, so a settlement
   that lowers a cost can never leave a counter too high. `agentd` also checks the run level when a
   model request is refused with 401 or 429, as it does for the other levels, so a run in flight
   records the limit and not the gateway's refusal.
6. **Tokens are reported, not enforced.** `runs.tokens_used` stays (input plus output as the gateway
   reports them). The run's usage by type (uncached input, cache read, cache write, output, and
   reasoning where the provider reports it) is summed from its ledger entries and returned on `Cost`
   with `GetRun` and `GetTask`. `limits.run.maxTokens` is **deprecated and ignored**: the template
   schema still accepts it, the proto fields `RunLimits.max_tokens` and `Run.max_tokens` stay
   (marked deprecated, like `asked_to` in old issue 232, so `buf breaking` passes), the column
   `runs.max_tokens` stays and is no longer written, and nothing cancels a run on it. Runs queued
   before this decision have no request or spend limit; they run to their time limit.
7. **The wrap-up notice covers all three limits** (old issue 183). The notice of ADR 0066 (an
   interrupted turn, then a prompt telling the agent to write up) fires once per run, for the first
   limit that reaches its window:
   - time: `WrapUpNotice` before the cap (5 minutes by default, so 83% of the default 30 minutes; a
     lead time because a write-up needs wall-clock time, and the same as before);
   - requests and spend: at 80% (`WrapUpFraction`) of the limit, read from the ledger every 5
     seconds while a turn runs (`UsagePoll`), and before a turn that starts inside the window.
   When two are in their window the nearer by fraction names the notice. The `run.wrap_up_notice`
   event carries the `limit` and the amounts; the prompt says which limit and how far. The ledger's
   cancel at the request or spend limit adds what the agent last said (`summary`), as the time-limit
   cancel does, so the reply is not lost.
8. **Overrides.** The template sets the default for each limit. An org admin overrides them per
   agent with `BudgetService.SetAgentRunLimits` (`agent_run_limits`, audited as
   `budget.agent_run_limits.set`), the way the agent's daily limit is overridden; an unset field
   follows the agent's version. A run copies its limits when it is queued, so a change applies to
   runs queued afterwards. A version made before this decision, with no run limits, gets the platform
   defaults: 200 requests and $5.00 per run. The same fallback applies to a template that omits
   `maxModelRequests` or `maxSpendUSD` (see the amendment on optional limits below).
9. **Defaults per template** are calibrated from the home-lab data below.

## Defaults, with the data

Requests per run and cost per run, from the rc.5 round (RT1, 2026-10-28, `claude-haiku-5.5` through
OpenRouter, the rc.5 build) and the rc.7 round (RT2, 2026-10-29, four models):

| Template | rc.5 requests (cost) | rc.7 requests (cost) | Largest | Default requests | Default spend |
| --- | --- | --- | --- | --- | --- |
| finops-analyst | 9, 11, 16 ($0.03 to $0.07) | 11 to 28 (haiku, luna, deepseek: $0.006 to $0.017); 15 on Sonnet 5.5 ($0.31) | 28 requests; $0.31 | **100** (3.6x) | **$0.75** (2.4x the dearest) |
| lcm-upgrader | 22, 34 ($0.10 to $0.17) | 11 to 37 ($0.009 to $0.062) | 37 requests; $0.17 | **150** (4x) | **$1.00** (6x) |
| researcher | internal 15, 16, 41 ($0.03 to $0.48); external 29, 33 ($1.11, $1.22, failed at the token cap) | internal 22 to 29 ($0.028 to $0.050); external 7 to 24 ($0.003 to $0.025) | 41 requests; $1.22 | **200** (4.9x) | **$2.00** (1.6x the uncached worst, 40x the cached) |
| repo-helper, repo-helper-gitea | no home-lab data | no home-lab data | n/a | **300** | **$5.00** |

The reasoning:

- **Requests** have headroom of 3.6 to 4.9 times the largest observed run. The data comes from small
  tasks on haiku-class models; a long coding run makes more requests, so the coding templates, with
  no home-lab data, are set at 300 on the assumption of a longer loop. A run that really needs more
  is an admin's override, not a reason to leave the guard loose for everyone.
- **Spend** is set against the dearest observed run for a template and the model classes an admin is
  likely to bind. The rc.5 researcher figures ($1.1 to $1.2) are from before prompt caching was
  always on (ADR 0068) and are the worst case the limit must not stop; the rc.7 figures for the same
  task are two orders of magnitude lower. Each run limit is below the template's daily `spendUSD`
  ($1, $2, $3 and $10), so one run cannot spend a whole day's budget and the daily limit stays the
  backstop for several runs.
- **The platform defaults** (200 requests, $5.00) are for versions made before this decision. They
  sit above every observed run and below the highest daily limit.
- The spend default depends on the model an admin binds: a template's $0.75 suits haiku-class and
  Sonnet-class runs of the observed size, and an admin who binds a dearer model sees the run limit
  next to the model's prices and raises it. The UI shows the effective limits on the agent page.

## Consequences

- The price catalog is now load-bearing for run stops, not only for budgets. A wrong price makes a
  run stop early or late; an unpriced model is already refused (ADR 0050). A model priced at an
  explicit $0 is bounded by the request limit and the time limit only.
- The request count depends on the harness: the title request, compaction summaries, the end-of-turn
  summary call and subagents all count. A new flavor needs calibration against its own loop.
- Standalone mode reloads the gateway config when a run starts and when it ends, because the key's
  limit changes with the run level. ADR 0073's per-run keys would cost the same.
- A run limit reached ends the run (cancel), not parks it: the limit exists to stop a loop, and a
  continuation belongs to old issue 183 with the per-task cap.
- The `max_tokens` field and column carry no meaning now. Removing them is a breaking change for a
  later API version.

## Alternatives considered

- **Price-weighted tokens**: correct with caching, but a cost-shaped unit that has neither the
  stability of a token count nor the meaning of money; "1M" is $0.10 on a small model and $4 on a
  large one.
- **Uncached input plus output**: fixes the symptom of old issue 243, but a cache miss (a TTL expiry after a
  park, a provider switch, a compaction rewrite) counts the whole context again, thinking is
  underweighted four to eight times against its cost, and it means something different on every
  tokenizer.
- **A request count alone**: the cheapest correct fix, but one request on a large-context model with a
  long thinking budget can cost dollars, so only the agent's daily limit would bound a run's money.
- **A counter column for requests and spend** on the run: cheaper to read, but a settlement that
  changes a cost would have to adjust it. Reading the entries (an index on `(org_id, run_id)`
  exists) cannot drift.

## Amendment 2026-10-29: review findings

1. **Reserves stay out of the run level.** A provisional entry (a response cut short, ADR 0056)
   reserves its request's `max_tokens` bound against the gateway key. That bound is priced for the
   whole possible output and can exceed an entire run limit (the finops default is $0.75): one cut
   response would push a run limit of zero for a run the ledger had not stopped, and the gateway would
   refuse the run's requests, the 80% wrap-up turn among them, with a 429 that `agentd` cannot tell
   from a budget stop (it compares charges only). The run level of the pushed limit is therefore the
   run's remaining *charges*. What a cheaper option would cost later: the run can overshoot its
   spend limit by the cost of the in-flight cut responses, which the settlement then checks (point
   2); the agent, org and platform levels still hold the reserve, so the key stays bounded by them.
   The overshoot is one in-flight response per run, the bound every level already has.
2. **One enforcement sequence for every writer of an entry.** Ingest and the settler both call
   `ledger.enforce`: the agent's, org's and platform's budgets (alerts, the stop, the reconciler
   wake), then `scheduler.CheckRunLimits`. A settlement that raises a cost past both the agent's
   daily limit and the run's spend limit therefore names the daily limit, as decision 5 says. The
   settler checks only when the settlement changed the entry, and its alerts go through the same
   `Notifier` as ingest's.
3. **The run's own cancel is one path.** `ledger.Stop` for a run-level limit (the reconciler's
   `StopSpent`) calls `scheduler.CheckRunLimits`, so the cancel carries the agent's last reply and the
   usage whichever path reaches the limit first (decision 7). `agentd`'s `budgetStop` records the
   limit and the reason; the ledger's cancel carries the reply when it wins the race.
4. **Attribution of a request to a run.** `FindRunAt` prefers the run that is executing (starting or
   running, claimed by the request's time) over a run that finished inside the grace. A parked run that
   resumes keeps its first `started_at`, older than the run that ran meanwhile, and its requests were
   booked to that finished run for up to the grace, so its request and spend counts came out too low and
   its limits were overshot. `claimed_at` is set on every claim, resume included. A response that
   completes after its run ended is still booked to that run when no run is executing, and a response
   logged before the resume is still booked to the run that was executing then.
5. **Serialisation.** Ingest locks the run's row before it writes the entry, and
   `CheckRunLimits` locks it (`FOR UPDATE`) before it reads the usage, holding the lock to the end of
   the transaction: concurrent requests of one run count each other's committed entries, so the request
   limit is exact, and the entry's foreign-key lock cannot deadlock with the token update.

## Amendment 2026-10-29: the request and spend limits are optional in a template

The first version of this decision made `limits.run.maxModelRequests` and `limits.run.maxSpendUSD`
required in the template schema. A custom template written before this decision, with only the
now-deprecated `maxTokens`, would then stop validating after an upgrade. That is a breaking upgrade
for a setting this ADR itself calls ignored.

1. **Both are optional.** The schema requires only `maxDuration`. A template that sets neither, or
   only `maxTokens`, validates. An explicit zero is still refused (the schema's minimum for requests,
   a `not` pattern for spend), so "unset" and "no limit" cannot be confused: there is no way to
   switch a limit off.
2. **An unset limit falls back to the platform default**, the one older agent versions already get:
   200 requests and $5.00 per run (`agentconfig.DefaultMaxModelRequests`, `DefaultMaxSpend`). The
   fallback is applied where a run's limits are read (`agentconfig.Config.RunLimits`), so a version
   stores what the template said and the platform's defaults can move later without rewriting
   versions. `TemplateService` reports the effective limits, defaults included.
3. **Overrides are unchanged.** An admin can still override either limit per agent
   (`BudgetService.SetAgentRunLimits`).
4. **The shipped templates keep their explicit, calibrated values.** The defaults are a floor for
   templates that predate run limits, not a replacement for calibration.

What a cheaper option would cost later: keeping the fields required would have forced every custom
template to be edited at upgrade time, and a hard default inside the schema would have hidden the
fallback from the version record.
