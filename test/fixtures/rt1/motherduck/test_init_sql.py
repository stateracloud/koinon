# SPDX-License-Identifier: Apache-2.0
"""16 concurrent connections to one database file each run the init SQL and a query.

Run: python3 -m unittest test_init_sql  (from this directory; needs the duckdb package, else skipped).
Regression test for koinon issue 62: with lock_configuration in the init SQL, all but one failed.
"""
import os
import tempfile
import threading
import unittest

from init_sql import INIT_SQL

try:
    import duckdb
except ImportError:  # pragma: no cover
    duckdb = None


class InitSQL(unittest.TestCase):
    def test_no_lock_configuration(self):
        self.assertNotIn("lock_configuration", INIT_SQL)
        self.assertIn("enable_external_access=false", INIT_SQL)

    @unittest.skipIf(duckdb is None, "duckdb is not installed")
    def test_sixteen_concurrent_connections(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "t.duckdb")
            con = duckdb.connect(path)
            con.execute("CREATE TABLE t AS SELECT 1 AS x")
            con.close()
            errors, results = [], []

            def worker():
                try:
                    c = duckdb.connect(path, read_only=True)
                    c.execute(INIT_SQL)
                    results.append(c.execute("SELECT sum(x) FROM t").fetchone()[0])
                except Exception as e:  # noqa: BLE001
                    errors.append(repr(e))

            threads = [threading.Thread(target=worker) for _ in range(16)]
            for t in threads:
                t.start()
            for t in threads:
                t.join()
            self.assertEqual(errors, [])
            self.assertEqual(len(results), 16)

    @unittest.skipIf(duckdb is None, "duckdb is not installed")
    def test_external_access_stays_off(self):
        con = duckdb.connect(":memory:")
        con.execute(INIT_SQL)
        with self.assertRaises(duckdb.Error):
            con.execute("SET enable_external_access=true")


if __name__ == "__main__":
    unittest.main()
