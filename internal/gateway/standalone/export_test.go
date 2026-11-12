// SPDX-License-Identifier: Apache-2.0

package standalone

// EscapeLoader is escapeLoader, for the specs that feed agentgateway a '$' no validated State can
// carry (#668). Test-only: no production caller escapes outside Render.
var EscapeLoader = escapeLoader
