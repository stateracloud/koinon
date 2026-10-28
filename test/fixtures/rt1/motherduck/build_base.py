# SPDX-License-Identifier: Apache-2.0
"""Build the base DuckDB file (image build time): download the FOCUS 1.0 sample, check its sha256, load it.

Usage: build_base.py URL SHA256 OUT.duckdb
Cost columns are scaled by 100 so the figures look like a small company's bill rather than a
sample's cents. The dates are shifted later, at container start (startup.py).
"""
import hashlib
import sys
import urllib.request

import duckdb

url, want, out = sys.argv[1:4]
data = urllib.request.urlopen(url, timeout=120).read()  # noqa: S310 - pinned https URL, checked below
got = hashlib.sha256(data).hexdigest()
if got != want:
    sys.exit(f"sha256 mismatch for {url}: got {got}, want {want}")
with open("/tmp/focus.csv", "wb") as f:
    f.write(data)

con = duckdb.connect(out)
con.execute(
    """
    CREATE TABLE focus AS
    SELECT * REPLACE (
        TRY_CAST(ConsumedQuantity AS DOUBLE) AS ConsumedQuantity,
        TRY_CAST(PricingQuantity AS DOUBLE) AS PricingQuantity,
        TRY_CAST(ContractedUnitPrice AS DOUBLE) AS ContractedUnitPrice,
        TRY_CAST(ListUnitPrice AS DOUBLE) AS ListUnitPrice,
        TRY_CAST(ContractedCost AS DOUBLE) * 100 AS ContractedCost,
        TRY_CAST(ListCost AS DOUBLE) * 100 AS ListCost,
        BilledCost * 100 AS BilledCost,
        EffectiveCost * 100 AS EffectiveCost
    )
    FROM read_csv_auto('/tmp/focus.csv', all_varchar = false)
    """
)
rows = con.execute("SELECT count(*) FROM focus").fetchone()[0]
if rows != 10000:
    sys.exit(f"expected 10000 rows, got {rows}")
con.close()
