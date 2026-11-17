-- SPDX-License-Identifier: Apache-2.0
--
-- A turn mark on the leak guard's open calls (#1057, ADR 0091). Expand-only: two nullable columns.
--
-- A call the guard passed stays open until the guard sees its reply. A call that the model makes in
-- the same turn cannot hold that reply: the model has not received it. The mark is the run and the
-- count of finished tool calls (leak_guard_runs.feed_read) when the call opened. Once the harness
-- finishes another tool call, a reply can have reached the model, and a later call counts the open
-- call as unseen. A call with no mark (no live run, no feed) always counts.

-- +goose Up

ALTER TABLE leak_guard_open_calls
    ADD COLUMN mark_run  uuid,
    ADD COLUMN mark_read bigint CHECK (mark_read >= 0);
