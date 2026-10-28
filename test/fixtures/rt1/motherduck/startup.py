# SPDX-License-Identifier: Apache-2.0
"""Container start: shift the sample's dates so the data ends yesterday, plant the anomaly, serve.

The base database in the image is never changed. A copy goes to a writable directory ($WORK,
an emptyDir in the pod), is shifted and then opened read-only by the MCP server. Nothing here
needs the network.

Planted anomaly: the cost of "Amazon Virtual Private Cloud" yesterday is set to three times its
average of the seven days before. The RT1 write-up checks the agent found it.
"""
import os
import shutil
import sys

import duckdb

BASE = os.environ.get("BASE_DB", "/opt/focus/base.duckdb")
WORK = os.environ.get("WORK", "/work")
SERVICE = "Amazon Virtual Private Cloud"
COST_COLUMNS = ("BilledCost", "EffectiveCost", "ListCost", "ContractedCost")

os.makedirs(os.environ.get("HOME", WORK), exist_ok=True)
db = os.path.join(WORK, "focus.duckdb")
shutil.copyfile(BASE, db)
con = duckdb.connect(db)
con.execute(
    """
    UPDATE focus SET
        ChargePeriodStart = ChargePeriodStart + to_days(s.d),
        ChargePeriodEnd = ChargePeriodEnd + to_days(s.d)
    FROM (SELECT date_diff('day', CAST(max(ChargePeriodStart) AS DATE),
                           CAST(now() AT TIME ZONE 'UTC' AS DATE) - 1) AS d FROM focus) AS s
    """
)
# Billing periods follow the charge period: calendar months, as in FOCUS.
con.execute(
    """
    UPDATE focus SET
        BillingPeriodStart = date_trunc('month', ChargePeriodStart),
        BillingPeriodEnd = date_trunc('month', ChargePeriodStart) + INTERVAL 1 MONTH
    """
)

yesterday = con.execute("SELECT CAST(max(ChargePeriodStart) AS DATE) FROM focus").fetchone()[0]
day = lambda: "CAST(ChargePeriodStart AS DATE)"  # noqa: E731
avg7 = con.execute(
    f"""
    SELECT sum(BilledCost) / 7 FROM focus
    WHERE ServiceName = ? AND {day()} BETWEEN ? - 7 AND ? - 1
    """,
    [SERVICE, yesterday, yesterday],
).fetchone()[0]
now_cost = con.execute(
    f"SELECT sum(BilledCost) FROM focus WHERE ServiceName = ? AND {day()} = ?", [SERVICE, yesterday]
).fetchone()[0]
if not avg7 or not now_cost:
    sys.exit(f"cannot plant the anomaly: avg7={avg7} yesterday={now_cost}")
factor = 3 * avg7 / now_cost
sets = ", ".join(f"{c} = {c} * {factor}" for c in COST_COLUMNS)
con.execute(f"UPDATE focus SET {sets} WHERE ServiceName = ? AND {day()} = ?", [SERVICE, yesterday])
con.execute("CHECKPOINT")
con.close()
print(f"focus.duckdb ready: data ends {yesterday}; {SERVICE} yesterday = 3 x its 7-day average", flush=True)

os.execvp(
    "mcp-server-motherduck",
    [
        "mcp-server-motherduck",
        "--transport", "http",
        "--host", "0.0.0.0",  # noqa: S104 - the pod's NetworkPolicy limits who can connect
        "--port", "8000",
        "--db-path", db,
        "--stateless-http",
    ],
)
