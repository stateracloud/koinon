-- SPDX-License-Identifier: Apache-2.0

-- +goose Up
-- Cache writes with a 1-hour TTL, priced at their own rate (ADR 0070 amendment, #270). The column
-- is a subset of cache_write_tokens, as in Anthropic's usage record: cache_write_tokens stays the
-- total of cache writes, so every existing sum and input split is unchanged, and the 5-minute
-- writes are cache_write_tokens - cache_write_1h_tokens. Existing entries were all priced at the
-- 5-minute rate, so 0 is their true value.
ALTER TABLE spend_entries
    ADD COLUMN cache_write_1h_tokens bigint NOT NULL DEFAULT 0
        CHECK (cache_write_1h_tokens >= 0 AND cache_write_1h_tokens <= cache_write_tokens);

-- +goose Down
ALTER TABLE spend_entries DROP COLUMN cache_write_1h_tokens;
