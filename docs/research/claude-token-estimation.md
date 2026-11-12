# Estimating Claude token counts offline

- Status: Complete
- Date: 2026-10-24
- Area: models-spend
- Tags: spend-ledger, models

Research date: 2026-10-24. Primary sources only: source at a pinned commit, plus official docs.
Each claim is tagged **[established]** (read directly in the cited source) or **[inference]** (my
conclusion from the sources). Source numbers like [S3] point to the Sources section at the end.

## Short answer

- Nobody ships an accurate offline tokenizer for current Claude models. Anthropic says its own
  public tokenizer is "no longer accurate" from Claude 3 on [S1]. **[established]**
- Claude Opus 4.7 and later (including the Fable, Mythos and 5.x models) use a newer tokenizer.
  It produces about 30% more tokens for the same text [S2][S3][S4]. Any fixed ratio has to be
  chosen per tokenizer generation. **[established]**
- The tools surveyed do one of three things:
  - a flat characters-per-token heuristic (OpenCode: 4 chars per token; Helicone: 0.25 tokens per
    char);
  - the legacy Anthropic tokenizer (Langfuse for every Claude model; LiteLLM for bare `claude-*`
    IDs except `claude-3*`);
  - tiktoken `cl100k_base` as a fallback (LiteLLM for Claude 3 and for Bedrock-style IDs).
  **[established]**
- The tools that bill or track cost (OpenCode, Langfuse's AI-gateway path) deliberately **do not**
  estimate cost when usage is missing. They record zero or nothing. **[established]**
- For Koinon: take `input_tokens` from the stream's `message_start` event whenever it arrived,
  because that number comes from the provider, not an estimate. Estimate only what is still
  missing. Use UTF-8 bytes ÷ 3.0 for older Claude models and bytes ÷ 2.3 for the Opus 4.7+
  tokenizer, and calibrate upward from real usage. Details below. **[inference]**

## 1. OpenCode (anomalyco/opencode, formerly sst/opencode; `@opencode/cli` 2.1.4)

- The whole estimator is `Math.round(input.length / 4)`: `CHARS_PER_TOKEN = 4`, applied to the
  JavaScript string length (UTF-16 code units). It is the same for every provider and model
  [S5 L3-5]. **[established]**
- It is used only for context management: sizing compaction and the output limit. It sums system
  parts, tools (name + description + JSON schema), text, reasoning, tool calls and tool results
  [S6 L1153-1183]. Images are a flat 1,500 tokens and PDFs a flat 2,000 [S6 L144-145].
  **[established]**
- OpenCode admits this undercounts. A comment says "about 4 characters per token, which can run
  low on dense text such as code", and the code adds a 15% safety margin (`ESTIMATE_ERROR = 0.15`)
  before computing the room left for output [S7 L51-52, L88]. **[established]**
- Cost always comes from provider usage. If an endpoint reports a cost, OpenCode uses it;
  otherwise it multiplies the reported tokens by catalog prices [S8 L45-53]. A missing usage
  object becomes zero tokens [S8 L11-19]. A failed step without a `finish` record publishes no
  usage at all [S9 L246-247]. **So an aborted stream costs $0 in OpenCode.** **[established]**

## 2. Langfuse (langfuse/langfuse)

- `tokenizerId === "claude"` routes to `countTokens` from `@anthropic-ai/tokenizer` ^0.0.4, run
  over the raw string or over `JSON.stringify` of the input or output [S10 L4, L52-59, L135-137;
  S11 L36]. **[established]**
- The default model table assigns `tokenizerId: "claude"` to all 37 Claude entries. That covers
  `claude-instant-1` through `claude-3-opus-20240229` and `claude-opus-5-5`; for example lines
  1335/1339 and 4774/4779 of [S12]. Claude 3+ and reasoning models are **not** skipped by model.
  **[established]**
- Langfuse skips tokenization in four cases: when usage or cost was provided, when the observation
  level is ERROR, or when the generation came through Langfuse's AI gateway. The code comment says
  that for failed or cancelled gateway calls "the true usage is unknown ... tokenizing it
  fabricates usage and cost" [S13 L1578-1589]. If tokenization fails, it leaves the counts empty
  rather than writing "a 0 or a bytes-based estimate" [S13 L1609-1612]. **[established]**
- So Langfuse tokenizes current Claude models with a tokenizer Anthropic says is inaccurate for
  them (§5). **[inference]**

## 3. LiteLLM (BerriAI/litellm)

- `uses_anthropic_tokenizer(model)` returns `model in litellm.anthropic_models and "claude-3" not
  in model` [S14 L2641-2642]. When it is true, LiteLLM loads the bundled legacy
  `anthropic_tokenizer.json` through Hugging Face `tokenizers` [S14 L2656-2662, L292-305;
  S15 L91-99]. **[established]**
- `anthropic_models` contains the IDs whose `litellm_provider` is `anthropic` [S16 L772-773]. In
  the current price map these include `claude-sonnet-4-5`, `claude-opus-4-7`, `claude-opus-5-5`
  and `claude-fable-5-1` [S17]. So Claude 4.x and 5.x get the **legacy** Claude 1/2 tokenizer,
  while Claude 3.x gets tiktoken. **[established; the "legacy" label inference from S1]**
- Every other case falls back to tiktoken. That includes provider-prefixed IDs such as
  `us.anthropic.claude-sonnet-4-6`, offline mode for non-bundled tokenizers, and a failed Hugging
  Face load [S14 L2618-2638]. The fallback is `cl100k_base` [S18 L44; S19 L745-755]. A unit test
  asserts that `us.anthropic.claude-sonnet-4-6` counts exactly match `cl100k_base`
  [S20 L104-109]. **[established]**
- LiteLLM can also call the provider's endpoint. The proxy's `/utils/token_counter` accepts
  `call_endpoint=True` to use "Anthropic or Google AI Studio Token Counting APIs" [S21
  L15192-15201], and cache-aware routing calls Anthropic `count_tokens` [S22 L151].
  **[established]**

## 4. Helicone and OpenRouter

- Helicone has no tokenizer. Its token-limit handler estimates `ceil(chars × 0.25)`, with 0.2 for
  `gpt-3.5-turbo`; there is no Claude-specific value [S23 L48-54, L348-365, L714-750]. Its docs
  call this "heuristic ... best-effort" [S24 L19, L117]. For streaming cost, Helicone tells users
  to turn on provider-reported stream usage [S25 L6]. **[established]**
- OpenRouter reports counts from "the model's native tokenizer", and credits are based on those
  counts [S26 L423; S27 L13, L65]. It describes no estimator. For Anthropic, aborting a stream
  "immediately stops model processing and billing". For unsupported providers (including AWS
  Bedrock and Google) and for non-streaming requests, "you will be billed for the complete
  response" [S28 L99-131]. **[established]**
- What is billed after an abort depends on the route. On Bedrock or Vertex, the full response may
  be billed even though the client saw only part of it. **[inference from S28]**

## 5. Anthropic first-party guidance

- **The tokenizer package.** The README of `@anthropic-ai/tokenizer` (v0.0.4, last commit
  2024-03-04) says: "As of the Claude 3 models, this algorithm is no longer accurate, but can be
  used as a very rough approximation. We suggest that you rely on `usage` in the response body
  wherever possible." [S1 L5] **[established]**
- **The token counting endpoint** (`/v1/messages/count_tokens`) is described as follows [S2]:
  - It is "**free to use**", with rate limits of 5,000 / 10,000 / 20,000 RPM for the Start /
    Build / Scale tiers, separate from message-creation limits [L1414-1426].
  - Its count "is an **estimate** ... might differ by a small amount" [L39].
  - It may include system-added tokens that are not billed [L41].
  - It does not apply prompt caching [L1430-1436].
  - It counts under the tokenizer of the `model` you pass [L1406].
  **[established]**
- **Tokenizer generations.** Opus 4.7 and later, the Fable and Mythos models, and Mythos Preview
  use a newer tokenizer with "approximately 30 percent more tokens" for the same text. Sonnet 4.6
  and earlier use the previous one [S2 L49, L1404-1410; S3 L54, L520]. **[established]**
- **Characters per token.**
  - Older tokenizer: "a token represents approximately 3.5 English characters" [S4 L65].
  - Current tokenizer: "1M tokens is roughly 555k words or 2.5M Unicode characters", which is
    about 2.5 chars per token. Earlier models fit about 750k words in 1M tokens [S29 L53].
  - Neither page gives a bytes-per-token figure. **[established]**
- **Fixed overheads you cannot see in the text:**
  - A tool-use system prompt is added when `tools` is set: for example 496 tokens on Haiku 4.5,
    675 on Opus 4.7 and 286 on Opus 5.5 [S30 L763-790].
  - Images cost `ceil(w/28) × ceil(h/28)` visual tokens [S31 L1263].
  **[established]**
- **Thinking.** Thinking is billed as output "even when the thinking text isn't returned". With
  `display: "omitted"` (the default on many models) the block is empty but billed the same. "The
  billed output token count does not match the count of tokens you see in the response."
  [S32 L15, L43, L519] **[established]** For a reasoning model, no text-based estimator can
  recover output from an aborted stream. **[inference]**
- **Streaming.** `message_start` carries a `usage` object with `input_tokens` and the cache fields
  [S33 L296, example L1402]. `message_delta` usage is cumulative [S33 L302]. **[established]** So
  once `message_start` has arrived, an aborted stream lacks only output tokens. **[inference]**

## 6. Measured comparisons

- I found no first-party comparison of tiktoken against Claude counts. **[established, by absence
  in S1-S4, S29]**
- **Reproducible from repo code (my measurement, small sample).** OpenCode commits recorded live
  Anthropic HTTP exchanges with real `usage` [S34]. Method:
  - Serialize `{system, messages, tools}` from each request body as JSON.
  - Count it with tiktoken 0.9.0, and with LiteLLM's bundled legacy `anthropic_tokenizer.json`
    through `tokenizers` 0.20.3.
  - Compare to `input_tokens + cache_read + cache_creation`, minus the documented 496-token
    tool-prompt overhead where `tools` was set.
  - Both samples are `claude-haiku-4-5-20251001` (older tokenizer) and use repetitive English
    filler.

  | Recording | Actual (adj.) | legacy Anthropic | cl100k | o200k | bytes/3.5 |
  |---|---|---|---|---|---|
  | keeps-a-long-tool-turn… (43,302 B, tools) | 11,797 | 10,618 (−10%) | 10,340 (−12%) | 10,087 (−14%) | 12,372 (+5%) |
  | writes-then-reads-cache… (24,664 B, no tools) | 5,761 | 5,304 (−8%) | 5,055 (−12%) | 4,804 (−17%) | 7,047 (+22%) |

  **[established as measured; two samples, synthetic text: indicative only]**
- In both samples, all three tokenizers **undercount** Claude by 8-17%. On the Opus 4.7+ tokenizer
  (+~30%), the undercount would be roughly 30-35%. Using a tokenizer as a cost estimate is
  therefore unsafe in exactly the direction Koinon cares about. **[inference]**

## 7. Recommendation for Koinon (all [inference])

**Order of preference for each request whose usage is missing:**

1. **Use provider-reported partial usage.** If `message_start` arrived, record its
   `input_tokens`, `cache_creation_input_tokens` and `cache_read_input_tokens` as reported (not
   "estimated"). Do the same with the last cumulative `message_delta.usage.output_tokens` seen.
2. **Estimate only the missing pieces with a calibrated byte ratio.** Do not port
   `@anthropic-ai/tokenizer` or use tiktoken for Claude: Anthropic disowns the first [S1], and
   both undercount (§6). Count the UTF-8 bytes of the serialized request parts (system, messages,
   tool definitions, tool results), then:
   - **Previous tokenizer** (Sonnet 4.6, Opus 4.6, Haiku 4.5 and earlier): `ceil(bytes / 3.0)`.
     The docs give about 3.5 chars per token [S4], and the sample above gave 3.5-4.3 bytes per
     token. Expected error: about +5% to +40% on English prose and JSON, near 0 to +15% on code.
   - **New tokenizer** (Opus 4.7+, Fable, Mythos, 5.x): `ceil(bytes / 2.3)`. The docs give about
     2.5 chars per token [S29] and ×1.3 versus the old tokenizer [S2]. Expected error: about 0% to
     +35%.
   - Add the documented fixed overheads: the tool-use system prompt per model [S30], images at
     `ceil(w/28)·ceil(h/28)` [S31], and PDFs at the page-based allowance from the PDF docs.
   - Measuring bytes rather than characters automatically overcounts non-Latin scripts. That
     matches the "overestimate is fine" requirement. I did not measure CJK or other non-Latin
     text, so the error there is unverified.
3. **Output on abort.** Estimate from the streamed `text_delta` and `input_json_delta` bytes with
   the same ratio. If the response had thinking blocks with empty or omitted text, the visible
   bytes say nothing about billed output [S32]. Record the visible estimate, set a
   `thinking_unmeasured` flag, and either:
   - reserve and settle against `max_tokens` (conservative); or
   - bound the estimate as elapsed generation time × a per-model observed output rate.
4. **Optional online reconciliation.** When a provider credential is reachable from the
   egress/gateway (never from the sandbox), `count_tokens` is free, uses its own rate limit, and
   is exact enough ("small" difference) for the input side [S2]. It can backfill input counts
   asynchronously. It is not needed for the offline path.

**Other providers.**

- OpenAI models: use an exact tiktoken port in Go (`o200k_base` for gpt-4o and later, `cl100k`
  for older), since that is the OpenAI tokenizer. I did not research a specific Go library here.
- Everything else (Gemini, Mistral, local models): use `ceil(bytes / 3.0)` plus the same
  calibration. This is safer than OpenCode's ÷4, which OpenCode itself says runs low [S7].

**Calibrate: yes.**

- For every request that does return usage, record `reported_input_tokens / request_bytes` per
  (provider, model or tokenizer generation), and the same for output.
- Keep a rolling high percentile, for example the p90 of tokens per byte over the last N requests.
- Use `max(static_ratio, learned_p90)`, so calibration can only make the estimate more
  conservative, never smaller than the documented baseline.
- Never feed estimated rows back into calibration.
- Store which ratio, and which version of it, produced each estimate, so spend reports can show
  "estimated (ratio v3, p90 0.41 tok/B)".

## Sources

Pinned commits:

- opencode `f20760b8bf3bced0fa5620b6afe7b78dd615ee36` (`github.com/sst/opencode` redirects to
  `anomalyco/opencode`, v2.1.4)
- langfuse `07e45044a5abe1cd265fafd0d7e8cfd78b04d806`
- litellm `e04e89c7010809cc051b14f0e43d71dd018cdaee`
- helicone `6b057cdaf302c7b79b1f1e99cbf1c26132bd59de`
- anthropic-tokenizer-typescript `4d75e64d32fe5cd2b5a24d954242f9fab12e8b4b`

Docs were fetched as Markdown on 2026-10-24. Line numbers refer to the `.md` rendering.

- S1 https://github.com/anthropics/anthropic-tokenizer-typescript/blob/4d75e64d32fe5cd2b5a24d954242f9fab12e8b4b/README.md#L5
- S2 https://platform.claude.com/docs/en/build-with-claude/token-counting (.md L39, L41, L49, L1404-1436)
- S3 https://platform.claude.com/docs/en/about-claude/pricing (.md L54, L520)
- S4 https://platform.claude.com/docs/en/about-claude/glossary (.md L65)
- S5 https://github.com/anomalyco/opencode/blob/f20760b8bf3bced0fa5620b6afe7b78dd615ee36/packages/core/src/util/token.ts#L3-L5
- S6 https://github.com/anomalyco/opencode/blob/f20760b8bf3bced0fa5620b6afe7b78dd615ee36/packages/core/src/session/compaction.ts#L144-L145 and #L1153-L1192
- S7 https://github.com/anomalyco/opencode/blob/f20760b8bf3bced0fa5620b6afe7b78dd615ee36/packages/core/src/session/model-request.ts#L51-L52 and #L86-L90
- S8 https://github.com/anomalyco/opencode/blob/f20760b8bf3bced0fa5620b6afe7b78dd615ee36/packages/core/src/session/usage.ts#L11-L53
- S9 https://github.com/anomalyco/opencode/blob/f20760b8bf3bced0fa5620b6afe7b78dd615ee36/packages/core/src/session/runner/step.ts#L246-L247
- S10 https://github.com/langfuse/langfuse/blob/07e45044a5abe1cd265fafd0d7e8cfd78b04d806/worker/src/features/tokenisation/usage.ts#L4-L137
- S11 https://github.com/langfuse/langfuse/blob/07e45044a5abe1cd265fafd0d7e8cfd78b04d806/worker/package.json#L36
- S12 https://github.com/langfuse/langfuse/blob/07e45044a5abe1cd265fafd0d7e8cfd78b04d806/worker/src/constants/default-model-prices.json#L1335-L1339 and #L4774-L4779
- S13 https://github.com/langfuse/langfuse/blob/07e45044a5abe1cd265fafd0d7e8cfd78b04d806/worker/src/services/IngestionService/index.ts#L1571-L1612
- S14 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/utils.py#L292-L305, #L2618-L2642, #L2656-L2662
- S15 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/rust_bridge/tokenizer.py#L91-L99
- S16 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/__init__.py#L772-L773
- S17 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/model_prices_and_context_window.json (entries with `"litellm_provider": "anthropic"`)
- S18 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/litellm_core_utils/default_encoding.py#L44
- S19 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/litellm_core_utils/token_counter.py#L745-L755
- S20 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/tests/unit/litellm_core_utils/test_token_counter.py#L104-L109
- S21 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/proxy/proxy_server.py#L15192-L15201
- S22 https://github.com/BerriAI/litellm/blob/e04e89c7010809cc051b14f0e43d71dd018cdaee/litellm/llms/anthropic/cache_aware_routing.py#L151
- S23 https://github.com/Helicone/helicone/blob/6b057cdaf302c7b79b1f1e99cbf1c26132bd59de/worker/src/lib/util/tokenLimitException.ts#L48-L54, #L348-L365, #L714-L750
- S24 https://github.com/Helicone/helicone/blob/6b057cdaf302c7b79b1f1e99cbf1c26132bd59de/docs/features/advanced-usage/token-limit-exception-handlers.mdx#L19
- S25 https://github.com/Helicone/helicone/blob/6b057cdaf302c7b79b1f1e99cbf1c26132bd59de/docs/faq/enable-stream-usage.mdx#L6
- S26 https://openrouter.ai/docs/api-reference/overview (.md L295, L423)
- S27 https://openrouter.ai/docs/use-cases/usage-accounting (.md L13, L65)
- S28 https://openrouter.ai/docs/api-reference/streaming (.md L99-L131)
- S29 https://platform.claude.com/docs/en/about-claude/models/overview (.md L53)
- S30 https://platform.claude.com/docs/en/agents-and-tools/tool-use/overview (.md L763-L790)
- S31 https://platform.claude.com/docs/en/build-with-claude/vision (.md L1263)
- S32 https://platform.claude.com/docs/en/build-with-claude/thinking (.md L15, L43, L519)
- S33 https://platform.claude.com/docs/en/build-with-claude/streaming (.md L296-L302, L1402)
- S34 https://github.com/anomalyco/opencode/tree/f20760b8bf3bced0fa5620b6afe7b78dd615ee36/packages/ai/test/fixtures/recordings/anthropic-messages-cache
  (`keeps-a-long-tool-turn-inside-the-cache-lookback.json`, `writes-then-reads-cache-control-on-identical-second-call.json`).
  Measurement scripts: scratchpad `tk.py`, `legacy.py`, run with `uv run --with tiktoken==0.9.0` / `--with tokenizers==0.20.3`.
