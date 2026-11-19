// SPDX-License-Identifier: Apache-2.0

// Package a2atools is the built-in gateway target of the remote agents (ADR 0123 decision 10): the
// three fixed tools send_task, get_task and cancel_task, on top of [a2aout]. The gateway runs it in its
// own process, so the tools do not change with an agent card. The text of a card never reaches a model
// (decision 11), the text of a remote reply is an untrusted tool result (decision 12), and every error
// that a model sees is a fixed text: a remote error body, a URL and a card never are part of it.
//
// It is a package of its own, and not part of mcpgateway, because a2aout builds its HTTP client with
// mcpgateway.NewTargetHTTPClient. The gateway takes the target through [mcpgateway.Options.Builtins].
package a2atools
