# SPDX-License-Identifier: Apache-2.0
"""The SQL mcp-server-motherduck runs on every connection it opens (--init-sql).

mcp-server-motherduck 1.1.0 opens a connection per query, and concurrent connections share one DuckDB
instance. `SET lock_configuration=true` makes every later SET fail on that instance, so the second
connection's init SQL raised "Cannot change configuration option lock_configuration" (11 of 12
concurrent queries failed in a reproduction; koinon issue 62). The init SQL must therefore be
idempotent: no lock_configuration. `enable_external_access=false` still cannot be switched back on,
because DuckDB refuses to re-enable it once it is off.
"""

INIT_SQL = "SET enable_external_access=false; SET memory_limit='96MB'; SET threads=2;"
