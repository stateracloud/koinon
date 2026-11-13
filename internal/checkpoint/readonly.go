// SPDX-License-Identifier: Apache-2.0

package checkpoint

// ReadOnlyError is the reason of a checkpoint request that restore-only mode (checkpoints.readOnly,
// ADR 0110 decision 4) fails, and the start of the reason with which the reconciler blocks a recreation
// that has no backup: koinon-agentd and the reconciler give the same words.
const ReadOnlyError = "restore-only checkpoints (checkpoints.readOnly): no backup is written"
