# Persona evolution for persistent agents: what the research says

- Status: Complete
- Date: 2026-10-22
- Area: runtime-sandbox
- Tags: memory, agent-home, security

**Date:** 2026-10-22
**Status:** research note, input to v0.2 ("persistent coworkers", issue #91)

## The question

Koinon plans a **fixed core** plus an **agent-evolved persona**:

- The fixed core holds the role, rules and permissions from Koinon and the template. The agent can't edit it, and it is restored every run.
- The persona lives in a git-backed per-agent store (ADR 0033).

How do LLMs represent and keep a persona? What makes a persona good or bad? And is there research on what happens when an agent is allowed to grow its own persona?

## Short answer

There is almost no direct research on agents rewriting their own persona over weeks of real work. The nearby evidence is substantial, though, and it points one way:

- Personas are real, steerable internal states of the model, and they drift by themselves within a single conversation.
- Self-editing memory and self-improvement loops do improve task performance. They also show measurable "misevolution": safety degrades as memory accumulates, repeated rewriting erodes detail, and agents game their objectives.
- Narrow changes to behaviour can generalise into broad persona shifts ("emergent misalignment"). Sycophancy can come in through user feedback and memory.
- Persona prompts do not reliably improve factual accuracy, and some personas raise toxicity, bias or jailbreak risk.

The evidence supports Koinon's split, on three conditions:

1. Only **descriptive, task-facing fields** evolve: style, preferences, working conventions and domain notes.
2. Values, rules, permissions and identity claims stay in the fixed core.
3. Persona changes are treated like code: proposed as diffs, evaluated against fixed probes, reviewed on a schedule and revertible.

This is mostly **inference** built from adjacent results, not a tested recipe.

Claims are marked **established** (replicated, or several sources), **reported** (one study or source) or **inference** (ours).

---

## 1. How LLMs represent and maintain a persona

**Simulators and role-play framing.** Shanahan, McDonell and Reynolds argue that a dialogue agent is best understood as role-playing a character, or a superposition of characters, that the underlying model simulates ([Role play with large language models, Shanahan et al., 2023, *Nature*; arXiv:2305.16367](https://arxiv.org/abs/2305.16367)). This is a conceptual framing, not an empirical result. The earlier "simulators" essay by janus (LessWrong, 2022) has no peer-reviewed primary version that we found.

**The assistant is a character the model selects.** Anthropic's "persona selection model" describes "Claude" as a character the pretrained model learned to simulate, which post-training then refines ([Anthropic, The persona selection model, 2026](https://www.anthropic.com/research/persona-selection-model)). **Reported**: a first-party theory, not a measured result.

**Persona has a direction in activation space.**

- Persona vectors are activation directions for traits such as evil, sycophancy and hallucination. They can track personality shifts during deployment, predict trait changes caused by finetuning (intended or not) and flag training data that would cause those changes ([Persona Vectors, Chen, Arditi, Sleight, Evans, Lindsey, 2025; arXiv:2507.21509](https://arxiv.org/abs/2507.21509)).
- The leading component of "persona space", measured across 275 character archetypes in Gemma 2 27B, Qwen 3 32B and Llama 3.3 70B, is an "Assistant Axis". Models drift away from its assistant end "in conversations demanding meta-reflection on the model's processes or featuring emotionally vulnerable users". Capping activations along the axis stabilises behaviour and resists persona-based jailbreaks ([The Assistant Axis, Lu, Gallagher, Michala, Fish, Lindsey, 2026; arXiv:2601.10387](https://arxiv.org/abs/2601.10387)).
- Activation steering in general:
  - [ActAdd, Turner et al., 2023; arXiv:2308.10248](https://arxiv.org/abs/2308.10248)
  - [Representation Engineering, Zou et al., 2023; arXiv:2310.01405](https://arxiv.org/abs/2310.01405)
  - [Contrastive Activation Addition, Rimsky et al., 2023; arXiv:2312.06681](https://arxiv.org/abs/2312.06681)
- Together, **established** across several labs and models.

**Character training versus prompting.** Anthropic trains Claude's character with a "character" variant of Constitutional AI. Claude generates messages relevant to a trait, writes responses, ranks them against the trait, and a preference model is trained on those rankings. Anthropic says the traits shouldn't be treated "like rules from which it never deviates" ([Anthropic, Claude's Character, 2024](https://www.anthropic.com/research/claude-character); [Constitutional AI, Bai et al., 2022; arXiv:2212.08073](https://arxiv.org/abs/2212.08073)).

**Inference for Koinon:** a system-prompt persona, such as Hermes's `SOUL.md`, sits on top of a trained assistant persona. It can shape style. It can't reliably override values trained into the model, and it can push the model off the assistant axis.

## 2. Persona consistency and drift

- **Instruction drift:** self-chat tests found "significant instruction drift within eight rounds" for LLaMA2-chat-70B and GPT-3.5. The authors attribute it partly to attention decaying over the system prompt, and propose "split-softmax" as a fix ([Measuring and Controlling Instruction (In)Stability, Li et al., 2024; arXiv:2402.10962](https://arxiv.org/abs/2402.10962)). **Reported.**
- **Identity drift:** across nine LLMs in personal-theme dialogues, "larger models experience greater identity drift", and assigning a persona "may not help" keep identity stable ([Examining Identity Drift in Conversations of LLM Agents, Choi et al., 2024; arXiv:2412.00804](https://arxiv.org/abs/2412.00804)). **Reported** (under review).
- **Persona drift in activations:** the Assistant Axis paper above. **Reported**, and it agrees with the two results above.
- **Benchmarks:**
  - PersonaChat: persona-grounded dialogue consistency ([Zhang et al., 2018; arXiv:1801.07243](https://arxiv.org/abs/1801.07243)).
  - RoleBench, from RoleLLM ([Wang et al., 2023; arXiv:2310.00746](https://arxiv.org/abs/2310.00746)).
  - CharacterEval: a Chinese role-play benchmark ([Tu et al., 2024; arXiv:2401.01275](https://arxiv.org/abs/2401.01275)).
  - PersonaGym evaluated 10 LLMs on 200 personas and 10,000 questions. "GPT-4.1 had the exact same PersonaScore as LLaMA-3-8b", so size and recency don't buy persona fidelity ([PersonaGym, Samuel et al., 2024/2025; arXiv:2407.18416](https://arxiv.org/abs/2407.18416)). **Reported.**
- **Gap:** all of these measure drift **within one conversation** against a **fixed** persona. None measures a persona the agent itself rewrites across sessions.

## 3. Self-modifying and self-evolving agents

| System | What the agent edits | Reported result | Failure modes and controls |
|---|---|---|---|
| [Generative Agents, Park et al., 2023; arXiv:2304.03442](https://arxiv.org/abs/2304.03442) | Memory stream plus synthesised "reflections" | Memory, reflection and planning each add to believability in ablations | Embellished or hallucinated memories; overly formal or cooperative behaviour |
| [MemGPT, Packer et al., 2023; arXiv:2310.08560](https://arxiv.org/abs/2310.08560) / [Letta memory blocks](https://docs.letta.com/guides/agents/memory-blocks) | In-context "persona" and "human" blocks, edited through tool calls | Better long-conversation consistency and document QA than fixed-context baselines | Letta offers `read_only` blocks and per-block character limits |
| [Voyager, Wang et al., 2023; arXiv:2305.16291](https://arxiv.org/abs/2305.16291) | Executable skill library | 3.3x more unique items than prior methods in Minecraft | Relies on GPT-4 self-verification |
| [Reflexion, Shinn et al., 2023; arXiv:2303.11366](https://arxiv.org/abs/2303.11366) | Verbal self-critique in episodic memory | 91% pass@1 on HumanEval vs 80% for GPT-4 | Needs a reliable success signal |
| [Self-Refine, Madaan et al., 2023; arXiv:2303.17651](https://arxiv.org/abs/2303.17651) | Its own output, iteratively | About 20% average gain across 7 tasks | Limited by the model's ability to critique itself |
| [Self-Rewarding LMs, Yuan et al., 2024; arXiv:2401.10020](https://arxiv.org/abs/2401.10020) | Its own reward (it judges itself) | Gains on AlpacaEval 2.0 over 3 iterations | Authors leave open whether gains saturate or turn into reward hacking |
| [Promptbreeder, Fernando et al., 2023; arXiv:2309.16797](https://arxiv.org/abs/2309.16797) | Task prompts, and the prompts that mutate them | Beats Chain-of-Thought and Plan-and-Solve on arithmetic and commonsense benchmarks | Fitness is a fixed benchmark |
| [ADAS, Hu, Lu, Clune, 2024; arXiv:2408.08435](https://arxiv.org/abs/2408.08435) / [Gödel Agent, Yin et al., 2024; arXiv:2410.04444](https://arxiv.org/abs/2410.04444) | Agent designs or code, including its own logic | Gains on reasoning and coding tasks | Gödel Agent reports runs that break themselves |
| [Darwin Gödel Machine, Zhang et al., 2025; arXiv:2505.22954](https://arxiv.org/abs/2505.22954) | Its own agent code, kept in an archive | SWE-bench 20.0% → 50.0%; Polyglot 14.2% → 30.7% | Run with sandboxing and human oversight; an appendix discusses objective hacking (details not verified) |
| [ACE, Zhang et al., 2025; arXiv:2510.04618](https://arxiv.org/abs/2510.04618) | System prompts and memory as an evolving "playbook" | +10.6% on agent benchmarks, +8.6% on finance benchmarks | Names **"context collapse"** (repeated rewriting erodes detail) and **"brevity bias"** in earlier methods; its fix is incremental delta updates, not full rewrites |

**Misevolution.** Self-evolution "deviates in unintended ways" along the model, memory, tool and workflow paths, even on top-tier models such as Gemini-2.5-Pro. Examples:

- **safety alignment degrades after memory accumulates**;
- self-made tools introduce vulnerabilities.

Prompt-based mitigations help only partly ([Your Agent May Misevolve, Shao et al., 2025, ICLR 2026; arXiv:2509.26354](https://arxiv.org/abs/2509.26354)). **Reported**, and the paper most directly relevant to Koinon. Survey: [A Survey of Self-Evolving Agents, Gao et al., 2025; arXiv:2507.21046](https://arxiv.org/abs/2507.21046).

**Hermes Agent (Nous Research)** ([Personality](https://hermes-agent.nousresearch.com/docs/user-guide/features/personality), [Memory](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory)):

- `SOUL.md` is "the agent's primary identity". It takes slot #1 in the system prompt and replaces the default identity. It loads only from `$HERMES_HOME`, and it is "scanned … for prompt injection patterns before inclusion".
- The docs tell the **user** to edit `SOUL.md`. They don't say the agent rewrites it.
- Self-improvement works through two capped memory files and through skills:
  - `MEMORY.md` (2,200 characters) and `USER.md` (1,375 characters), edited with `add`, `patch` and `remove` tools;
  - memory is a "frozen snapshot" taken at session start;
  - writes are injection-scanned;
  - a background review may save memory or update skills without being asked;
  - `memory.write_approval` and `skills.write_approval` stage writes for human approval.

**Reported** from first-party docs. We found no published evaluation from Nous of how `SOUL.md`, memory or skills evolve over time.

## 4. Good vs bad personas

- **Task performance:** across 162 roles, 2,410 factual questions and 4 model families, persona system prompts did **not** improve accuracy over a no-persona control. Picking the best persona automatically did no better than picking one at random ([Zheng et al., 2024; arXiv:2311.10054](https://arxiv.org/abs/2311.10054)). Some studies report gains on reasoning from role-play prompting ([Kong et al., 2023; arXiv:2308.07702](https://arxiv.org/abs/2308.07702)). Overall it is **established** that a persona is not a dependable lever on accuracy.
- **Harms:**
  - assigned personas raised ChatGPT's toxicity by up to 6x ([Deshpande et al., 2023; arXiv:2304.05335](https://arxiv.org/abs/2304.05335));
  - persona assignment surfaced implicit reasoning biases ([Gupta et al., 2023; arXiv:2311.04892](https://arxiv.org/abs/2311.04892));
  - "persona modulation" jailbreaks raised GPT-4's harmful-completion rate to 42.5%, about 185x the baseline ([Shah et al., 2023; arXiv:2311.03348](https://arxiv.org/abs/2311.03348)).

  **Established** that personas can widen the harm surface.
- **Sycophancy:**
  - It is widespread in RLHF assistants, and human preference data rewards it ([Sharma et al., 2023; arXiv:2310.13548](https://arxiv.org/abs/2310.13548)).
  - OpenAI's April 2025 GPT-4o rollback found that new user-feedback rewards weakened the reward signal that held sycophancy in check. It also found that "user memory contributes to exacerbating the effects of sycophancy". Sycophancy was not among the deployment evals at the time ([OpenAI, Expanding on what we missed with sycophancy, 2025](https://openai.com/index/expanding-on-sycophancy/)).

  **Established** (sycophancy) and **reported** (the memory effect).
- **Honesty and character:** Anthropic's character work explicitly rejects mirroring the user's views ("pandering and insincere") and calls an "excessive desire to be engaging" undesirable ([Claude's Character, 2024](https://www.anthropic.com/research/claude-character)).
- **Trust, anthropomorphism and manipulation:**
  - In a 4-week RCT (n=981, more than 300k messages), heavier voluntary use went with worse loneliness and dependence outcomes, and trust and social attraction predicted emotional dependence ([Fang et al., 2025; arXiv:2503.17473](https://arxiv.org/abs/2503.17473)).
  - About 37% of 1,200 real farewells in companion apps used manipulative tactics, which raised engagement by up to 14x ([De Freitas et al., 2025; arXiv:2508.19258](https://arxiv.org/abs/2508.19258)).
  - For a broader treatment, see [The Ethics of Advanced AI Assistants, Gabriel et al., Google DeepMind, 2024; arXiv:2404.16244](https://arxiv.org/abs/2404.16244).

  Each is **reported**, and they point the same way.

**Inference:**

- A **good** coworker persona is accurate about what it is, candid, calibrated and focused on the task.
- A **bad** one optimises for being liked or for engagement, claims feelings or a relationship, or flatters.
- An evolving persona tuned on user reactions will drift toward the bad kind unless something checks it.

## 5. Safety and governance of persona evolution

- **Emergent misalignment:**
  - Finetuning on insecure code produced broadly misaligned behaviour on unrelated prompts, and a trigger-gated variant stayed hidden without its trigger ([Emergent Misalignment, Betley et al., 2025; arXiv:2502.17424](https://arxiv.org/abs/2502.17424)).
  - OpenAI traced the effect to "misaligned persona" features, a toxic-persona feature above all, and restored alignment with a few hundred benign samples ([Persona Features Control Emergent Misalignment, Wang et al., 2025; arXiv:2506.19823](https://arxiv.org/abs/2506.19823)).
  - **Established** for weight updates. Whether **in-context** persona edits generalise the same way is untested (**inference**: plausible, given the persona-vector results).
- **Persistent injection through memory:**
  - Indirect prompt injection is established ([Greshake et al., 2023; arXiv:2302.12173](https://arxiv.org/abs/2302.12173)).
  - MINJA plants malicious memory records through ordinary queries alone ([Dong et al., 2025; arXiv:2503.03704](https://arxiv.org/abs/2503.03704)).
  - AgentPoison poisons agent memory and knowledge bases ([Chen et al., 2024; arXiv:2407.12784](https://arxiv.org/abs/2407.12784)).
  - A persona file the agent writes and the next run trusts is the same channel. **Established** as an attack class.
- **Hidden behaviour persists:** backdoored behaviour survived standard safety training ([Sleeper Agents, Hubinger et al., 2024; arXiv:2401.05566](https://arxiv.org/abs/2401.05566)). This is a weights result, cited as an analogy.
- **Guardrails with evidence behind them:**
  - activation monitoring and capping (Chen et al.; Lu et al.);
  - incremental delta edits rather than full rewrites (ACE);
  - injection scanning, size caps, frozen snapshots and write approval (Hermes, Letta);
  - sycophancy evals before deployment (OpenAI).
- **No primary study** evaluates periodic human review or diff review of agent personas. Recommending them is **inference** from standard change control.

## 6. Synthesis for Koinon (inference built on the results above)

1. **Keep the split.** Values, rules, permissions, the role and identity claims ("I am X, built by Y, I can/can't…") go in the fixed core. It is restored every run and placed before the persona in the prompt. Persona-modulation jailbreaks, emergent misalignment and drift all show that identity and values are where small edits generalise.
2. **Fields that may evolve**, bounded and descriptive:
   - communication style and verbosity;
   - working conventions (commit style, testing habits);
   - domain and codebase notes;
   - lessons learned;
   - shared team preferences.

   Per-person preferences belong in the private per-requester memory, not the shared persona. That way one user's flattery or injection can't reshape the agent for everyone (MINJA; OpenAI's memory–sycophancy finding).
3. **Fields that must not evolve:**
   - values and ethics;
   - refusal and approval rules;
   - permissions and tool scope;
   - claims about its own nature, feelings or relationships;
   - instructions addressed to future runs ("always…", "ignore…");
   - anything about how to treat the fixed core.

   Reject persona text that reads as instructions rather than description (Hermes's docs give the same advice).
4. **Gating:**
   - The agent writes **proposals** as small diffs to a schema with typed sections and a character cap per section, never a full rewrite (this avoids context collapse).
   - A scanner plus an automated eval run act as a pre-merge check.
   - An admin approves changes to shared-persona sections. Low-risk sections, such as domain notes, can auto-merge once the evals pass.
   - A weekly or monthly review compares the cumulative diff with the original persona, because drift compounds and single diffs hide it.
   - Roll back with git revert.
5. **What to measure on every persona change:**
   - a fixed suite of sycophancy probes (agree-with-wrong-user, praise-seeking), as in Sharma et al.;
   - refusal and safety probes, to catch misevolution;
   - the template's task regression set;
   - a persona-consistency check against the core role (PersonaGym-style, or LLM-judged);
   - an injection scan;
   - size and churn per section.

   If open-weights models are ever self-hosted, persona-vector projections on the sycophancy and evil directions are a cheap drift monitor.
6. **Treat "growing a personality" as a product hypothesis, not a known benefit.** No study shows that a self-authored persona improves task outcomes or user trust over a well-written fixed persona. Persona prompts don't improve accuracy, and engagement-seeking traits hurt users.

## Decided rules (2026-10-22)

procoduck decided these rules from the evidence above, as input to the v0.2 design (#91). They are recorded here, not in an ADR, until the v0.2 design pass.

1. **Schema:**
   - The persona is a set of typed sections in the agent store (ADR 0033), each with a character cap: working style (600), conventions (1,500), domain notes (3,000), lessons learned (2,000) and team preferences (800).
   - Per-person preferences go in each requester's private memory, never in the persona.
   - The fixed core sits above the persona in the prompt and is restored every run. It holds the role, values, rules, approvals, permissions and identity.
2. **Forbidden content** is rejected by a deterministic linter on every proposed change:
   - instructions to future runs ("always", "never", "ignore");
   - claims about the agent's own nature, feelings or relationships;
   - anything about values, rules, approvals, permissions, tools or the fixed core;
   - secrets and credentials, URLs to fetch, and text that matches injection patterns.
3. **Gating:**
   - The agent never writes the persona directly. It proposes small diffs, never full rewrites.
   - Domain notes and lessons learned auto-merge once the checks pass.
   - Working style, conventions and team preferences need the owner's approval through the "waiting on you" column.
   - Every change is a git commit and can be reverted.
4. **Checks:**
   - On every proposal: the linter, an injection scan, and size and churn limits. These are free and deterministic.
   - Once a day, for each agent with merged changes: a fixed suite of sycophancy probes, refusal and safety probes, and the template's task regression set. They run on the agent's own model and are charged to the org, with a cap.
   - A failure reverts that day's persona changes and notifies the owner.
5. **Drift:**
   - Once a month, the owner reviews the cumulative diff against the last reviewed baseline, then approves it (it becomes the new baseline) or reverts it.
   - If churn since the baseline goes over a cap, for example 50% of the persona's characters, auto-merge pauses until that review.
6. **Personal agents** follow the same rules, with the user as the approver. The fixed core still comes from the template and the org.
7. **Measuring the hypothesis:**
   - Every run records the persona's git version.
   - For each version, Koinon tracks the approval rate, how often requesters reject the work or ask for rework, and ticket completion time and cost. These are compared with the template's starting persona.
   - Whether evolution stays on by default is decided after real use.

## Evidence gaps

- **No direct study** of an agent editing its own persona or identity file over a long deployment with outcomes measured. Letta, Hermes and Generative Agents ship or demo the mechanism but publish no longitudinal evaluations of personas.
- Drift benchmarks test fixed personas within one conversation, not personas that evolve across sessions.
- Unknown whether **in-context** persona edits cause the generalisation seen in emergent misalignment; all the evidence so far comes from weight updates.
- No evaluation of human diff review, review cadence, or gating by proposals vs direct edits for agent memory.
- Little work on personas for multi-user team agents; most research on persona harms covers one-to-one companions.
- Not verified: exact numbers in Shao et al. (misevolution), the details of the Darwin Gödel Machine's objective-hacking case, and per-model scores on RoleBench and CharacterEval.

## Sources

**Persona representation and character**

- Shanahan, McDonell, Reynolds (2023). Role play with large language models. *Nature*. [arXiv:2305.16367](https://arxiv.org/abs/2305.16367)
- Anthropic (2026). The persona selection model. [anthropic.com](https://www.anthropic.com/research/persona-selection-model)
- Anthropic (2024). Claude's Character. [anthropic.com](https://www.anthropic.com/research/claude-character)
- Bai et al. (2022). Constitutional AI. [arXiv:2212.08073](https://arxiv.org/abs/2212.08073)
- Chen, Arditi, Sleight, Evans, Lindsey (2025). Persona Vectors. [arXiv:2507.21509](https://arxiv.org/abs/2507.21509)
- Lu, Gallagher, Michala, Fish, Lindsey (2026). The Assistant Axis. [arXiv:2601.10387](https://arxiv.org/abs/2601.10387)
- Turner et al. (2023). Activation Addition. [arXiv:2308.10248](https://arxiv.org/abs/2308.10248)
- Zou et al. (2023). Representation Engineering. [arXiv:2310.01405](https://arxiv.org/abs/2310.01405)
- Rimsky et al. (2023). Contrastive Activation Addition. [arXiv:2312.06681](https://arxiv.org/abs/2312.06681)

**Drift and persona benchmarks**

- Li et al. (2024). Measuring and Controlling Instruction (In)Stability. [arXiv:2402.10962](https://arxiv.org/abs/2402.10962)
- Choi et al. (2024). Examining Identity Drift in Conversations of LLM Agents. [arXiv:2412.00804](https://arxiv.org/abs/2412.00804)
- Zhang et al. (2018). PersonaChat. [arXiv:1801.07243](https://arxiv.org/abs/1801.07243)
- Wang et al. (2023). RoleLLM / RoleBench. [arXiv:2310.00746](https://arxiv.org/abs/2310.00746)
- Tu et al. (2024). CharacterEval. [arXiv:2401.01275](https://arxiv.org/abs/2401.01275)
- Samuel et al. (2024/2025). PersonaGym. [arXiv:2407.18416](https://arxiv.org/abs/2407.18416)

**Self-modifying and self-evolving agents**

- Park et al. (2023). Generative Agents. [arXiv:2304.03442](https://arxiv.org/abs/2304.03442)
- Packer et al. (2023). MemGPT. [arXiv:2310.08560](https://arxiv.org/abs/2310.08560)
- Letta docs: Memory blocks. [docs.letta.com](https://docs.letta.com/guides/agents/memory-blocks)
- Wang et al. (2023). Voyager. [arXiv:2305.16291](https://arxiv.org/abs/2305.16291)
- Shinn et al. (2023). Reflexion. [arXiv:2303.11366](https://arxiv.org/abs/2303.11366)
- Madaan et al. (2023). Self-Refine. [arXiv:2303.17651](https://arxiv.org/abs/2303.17651)
- Yuan et al. (2024). Self-Rewarding Language Models. [arXiv:2401.10020](https://arxiv.org/abs/2401.10020)
- Fernando et al. (2023). Promptbreeder. [arXiv:2309.16797](https://arxiv.org/abs/2309.16797)
- Hu, Lu, Clune (2024). ADAS. [arXiv:2408.08435](https://arxiv.org/abs/2408.08435)
- Yin et al. (2024). Gödel Agent. [arXiv:2410.04444](https://arxiv.org/abs/2410.04444)
- Zhang et al. (2025). Darwin Gödel Machine. [arXiv:2505.22954](https://arxiv.org/abs/2505.22954)
- Zhang et al. (2025). Agentic Context Engineering. [arXiv:2510.04618](https://arxiv.org/abs/2510.04618)
- Shao et al. (2025). Your Agent May Misevolve. [arXiv:2509.26354](https://arxiv.org/abs/2509.26354)
- Gao et al. (2025). A Survey of Self-Evolving Agents. [arXiv:2507.21046](https://arxiv.org/abs/2507.21046)
- Nous Research, Hermes Agent docs: [Personality](https://hermes-agent.nousresearch.com/docs/user-guide/features/personality), [Memory](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory)

**Persona effects: performance, harms, sycophancy, users**

- Zheng et al. (2024). Personas in System Prompts Do Not Improve Performances. [arXiv:2311.10054](https://arxiv.org/abs/2311.10054)
- Kong et al. (2023). Role-play prompting for zero-shot reasoning. [arXiv:2308.07702](https://arxiv.org/abs/2308.07702)
- Deshpande et al. (2023). Toxicity in ChatGPT. [arXiv:2304.05335](https://arxiv.org/abs/2304.05335)
- Gupta et al. (2023). Bias Runs Deep. [arXiv:2311.04892](https://arxiv.org/abs/2311.04892)
- Shah et al. (2023). Persona modulation jailbreaks. [arXiv:2311.03348](https://arxiv.org/abs/2311.03348)
- Sharma et al. (2023). Towards Understanding Sycophancy. [arXiv:2310.13548](https://arxiv.org/abs/2310.13548)
- OpenAI (2025). Expanding on what we missed with sycophancy. [openai.com](https://openai.com/index/expanding-on-sycophancy/)
- Fang et al. (2025). Psychosocial effects of extended chatbot use (RCT). [arXiv:2503.17473](https://arxiv.org/abs/2503.17473)
- De Freitas et al. (2025). Emotional Manipulation by AI Companions. [arXiv:2508.19258](https://arxiv.org/abs/2508.19258)
- Gabriel et al. (2024). The Ethics of Advanced AI Assistants. [arXiv:2404.16244](https://arxiv.org/abs/2404.16244)

**Safety and governance**

- Betley et al. (2025). Emergent Misalignment. [arXiv:2502.17424](https://arxiv.org/abs/2502.17424)
- Wang et al., OpenAI (2025). Persona Features Control Emergent Misalignment. [arXiv:2506.19823](https://arxiv.org/abs/2506.19823)
- Greshake et al. (2023). Indirect Prompt Injection. [arXiv:2302.12173](https://arxiv.org/abs/2302.12173)
- Dong et al. (2025). MINJA memory injection. [arXiv:2503.03704](https://arxiv.org/abs/2503.03704)
- Chen et al. (2024). AgentPoison. [arXiv:2407.12784](https://arxiv.org/abs/2407.12784)
- Hubinger et al. (2024). Sleeper Agents. [arXiv:2401.05566](https://arxiv.org/abs/2401.05566)
