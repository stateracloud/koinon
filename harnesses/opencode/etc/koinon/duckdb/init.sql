-- SPDX-License-Identifier: Apache-2.0
--
-- DuckDB defaults for the agent sandbox. The duckdb wrapper (koinon-duckdb) passes this file to the
-- CLI as -cmd. The sandbox VM has 1-2 GiB and no network. A query that does not fit spills to
-- temp_directory instead of taking the VM down; an agent may change any of these for its own
-- query (the configuration is not locked).
SET memory_limit = '768MiB';
SET threads = 2;
SET temp_directory = '/tmp/duckdb';
SET max_temp_directory_size = '1GiB';
-- No extension downloads (there is no network); the built-in readers (CSV, JSON, Parquet) work.
SET autoinstall_known_extensions = false;
SET autoload_known_extensions = false;
SET allow_community_extensions = false;
