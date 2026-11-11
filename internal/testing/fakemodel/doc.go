// SPDX-License-Identifier: Apache-2.0

// Package fakemodel is a scripted, deterministic model server for tests (ADR 0025). CI never
// calls paid models; it points harnesses, agentgateway and Koinon's own code at this server.
//
// It speaks two wire formats:
//
//   - OpenAI-compatible: POST /v1/chat/completions (JSON, or SSE when "stream": true, with a
//     final usage chunk when "stream_options": {"include_usage": true}) and GET /v1/models.
//   - Anthropic-compatible: POST /v1/messages (JSON, or an SSE event stream of message_start,
//     content_block_start/delta/stop, message_delta with cumulative usage, and message_stop).
//     GET /v1/models answers in Anthropic's list shape when the request carries an
//     anthropic-version header.
//
// GET /_fake/requests lists the path and model of every request received (for tests that reach the
// binary over the network).
//
// Responses come from a [Script] selected by the request's model name. A script is an ordered
// list of [Turn]s; for each request the first turn whose [When] condition holds is answered.
// Selection is stateless (it looks only at the request's conversation), so retries and parallel
// tests see the same answer. A script may also inject [Faults]: HTTP 503 for the first N
// requests, a delay before answering, and a truncated response (the connection is aborted
// mid-body). Further modes (FM2): rateLimit answers the first N requests with HTTP 429 and a
// Retry-After header; contextLength answers 400 context_length_exceeded (OpenAI) or "prompt is
// too long" (Anthropic); overloadedMidStream ends a stream with an overloaded error event after
// about half the events; stallMidStream holds a stream open after about half the events until the
// client closes it; splitWrites writes each SSE event in small flushed pieces; lockstep holds each request until the test calls [Server.Release]
// ([Server.Waiting] counts the held ones; lockstep works only in-process, because the
// binary has no release endpoint); refuse answers the first N requests with refuseStatus (a client
// error, 400 by default), and doNotRetry marks every scripted fault with x-should-retry: false.
// Unknown models get HTTP 404 with the API's native error body.
//
// Scripts load from YAML files ([LoadDir], [ParseScript]) or are registered from Go
// ([Server.Register]). The server is an [http.Handler], so tests run it in-process with
// net/http/httptest; cmd/koinon-fakemodel serves it as a binary.
//
// Wire formats were checked on 2026-10-22 against the public API references:
//
//   - OpenAI chat completions: https://platform.openai.com/docs/api-reference/chat/create
//   - OpenAI streaming chunk object (chat.completion.chunk, usage null except on the last chunk,
//     which has empty choices): https://platform.openai.com/docs/api-reference/chat-streaming
//   - OpenAI models list: https://platform.openai.com/docs/api-reference/models/list
//   - OpenAI error codes: https://platform.openai.com/docs/guides/error-codes
//   - Anthropic Messages: https://platform.claude.com/docs/en/api/messages
//   - Anthropic streaming events (event flow, cumulative usage in message_delta, ping, error
//     events, text_delta and input_json_delta): https://platform.claude.com/docs/en/build-with-claude/streaming
//   - Anthropic errors: https://platform.claude.com/docs/en/api/errors
//   - Anthropic models list: https://platform.claude.com/docs/en/api/models-list
package fakemodel
