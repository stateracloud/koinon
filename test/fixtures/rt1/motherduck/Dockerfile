# SPDX-License-Identifier: Apache-2.0
#
# RT1 fixture: a DuckDB database over the FinOps Foundation's FOCUS 1.0 sample data (CC BY 4.0, see
# NOTICE), served read-only as a remote MCP server by mcp-server-motherduck (MIT) over Streamable HTTP
# on :8000/mcp. The data is baked in at build time, so the pod needs no egress at run time. At start
# startup.py shifts the dates so they end yesterday and plants the anomaly.
#
# Build (from this directory): docker buildx build --platform linux/amd64 -t <registry>/koinon/rt1-motherduck:<tag> .
# python:3.12-slim, re-pin with: docker buildx imagetools inspect python:3.12-slim
ARG PYTHON=python:3.12-slim@sha256:12316debbfb86890f6b2d825820612741ba527ad5a44e3902759141d572bfe8f

FROM ${PYTHON} AS build
# The FOCUS sample data, pinned by commit and checked by sha256 (focus_sample_10000.csv, 10,000 rows).
ARG FOCUS_COMMIT=139a302bdaa2eb84a86becb82056488a5528a672
ARG FOCUS_SHA256=0bf58b73123294c4476b648e647cad842935036f67dc4e9a2cc4a6d7497cc2e3
COPY constraints.txt /tmp/constraints.txt
RUN python -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir -c /tmp/constraints.txt mcp-server-motherduck==1.1.0 duckdb==1.5.6
COPY build_base.py /tmp/build_base.py
RUN mkdir -p /opt/focus \
 && /opt/venv/bin/python /tmp/build_base.py \
    "https://raw.githubusercontent.com/FinOps-Open-Cost-and-Usage-Spec/FOCUS-Sample-Data/${FOCUS_COMMIT}/FOCUS-1.0/focus_sample_10000.csv" \
    "${FOCUS_SHA256}" /opt/focus/base.duckdb

FROM ${PYTHON}
COPY --from=build /opt/venv /opt/venv
COPY --from=build /opt/focus /opt/focus
COPY NOTICE /opt/focus/NOTICE
COPY startup.py /opt/startup.py
ENV PATH=/opt/venv/bin:$PATH \
    HOME=/work/home \
    WORK=/work \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
# /work is the only writable path: mount an emptyDir there when the root file system is read-only.
RUN mkdir -p /work/home && chown -R 10001:10001 /work
USER 10001:10001
EXPOSE 8000
ENTRYPOINT ["python", "/opt/startup.py"]
