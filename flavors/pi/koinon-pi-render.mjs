// SPDX-License-Identifier: Apache-2.0
//
// koinon-pi-render renders the pi flavor's configuration: the read-only templates in
// /etc/koinon/pi/ into pi's agent directory (PI_CODING_AGENT_DIR), with @KOINON_*@
// placeholders replaced by the per-agent values from koinon-pi's arguments.
//
// pi substitutes environment variables only in apiKey and headers, not in baseUrl, model ids,
// defaultModel or MCP urls, and it takes a lock file next to settings.json even to read it, so
// its agent directory must be writable and the values must be written in. Substitution is
// structural (string values of the parsed JSON, never the text), so a value cannot change the
// document's shape. Every file is parsed back and checked before pi starts: the spike shipped an
// empty mcp.json once, and pi then silently ran without the Koinon tools (ADR 0048, spike 7).
//
// Usage: node koinon-pi-render.mjs TEMPLATE_DIR AGENT_DIR (values in KOINON_MODEL_BASE_URL,
// KOINON_MODEL, KOINON_MCP_URL).

import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const [templateDir, agentDir] = process.argv.slice(2);
if (!templateDir || !agentDir) fail("usage: koinon-pi-render TEMPLATE_DIR AGENT_DIR");

const values = {};
for (const name of ["KOINON_MODEL_BASE_URL", "KOINON_MODEL", "KOINON_MCP_URL"]) {
  const v = process.env[name];
  if (!v) fail(`${name} is not set`);
  values[name] = v;
}

function fail(msg) {
  process.stderr.write(`koinon-pi-render: ${msg}\n`);
  process.exit(65);
}

// render replaces whole-string placeholders "@NAME@" in string values, recursively.
function render(node, file) {
  if (typeof node === "string") {
    const m = /^@([A-Z_]+)@$/.exec(node);
    if (!m) {
      if (node.includes("@KOINON_")) fail(`${file}: placeholder must be a whole value: ${node}`);
      return node;
    }
    if (!(m[1] in values)) fail(`${file}: unknown placeholder ${node}`);
    return values[m[1]];
  }
  if (Array.isArray(node)) return node.map((n) => render(n, file));
  if (node && typeof node === "object") {
    return Object.fromEntries(Object.entries(node).map(([k, v]) => [k, render(v, file)]));
  }
  return node;
}

const checks = {
  "models.json": (c) => {
    const p = c?.providers?.koinon;
    return p?.baseUrl === values.KOINON_MODEL_BASE_URL && p?.models?.some((m) => m.id === values.KOINON_MODEL);
  },
  "settings.json": (c) => c?.defaultProvider === "koinon" && c?.defaultModel === values.KOINON_MODEL,
  "mcp.json": (c) => {
    const s = c?.mcpServers?.koinon;
    return s?.url === values.KOINON_MCP_URL && s?.exposure === "direct" && typeof s?.headers?.Authorization === "string";
  },
};

mkdirSync(agentDir, { recursive: true, mode: 0o700 });
for (const [file, check] of Object.entries(checks)) {
  let template;
  try {
    template = JSON.parse(readFileSync(join(templateDir, file), "utf8"));
  } catch (err) {
    fail(`${file}: template: ${err.message}`);
  }
  const text = JSON.stringify(render(template, file), null, 2) + "\n";
  const dest = join(agentDir, file);
  const tmp = `${dest}.koinon-tmp`;
  writeFileSync(tmp, text, { mode: 0o600 });
  renameSync(tmp, dest);
  // Read back what pi will read.
  let written;
  try {
    written = JSON.parse(readFileSync(dest, "utf8"));
  } catch (err) {
    fail(`${file}: rendered file is not JSON: ${err.message}`);
  }
  if (!written || typeof written !== "object" || Object.keys(written).length === 0) {
    fail(`${file}: rendered file is empty`);
  }
  if (!check(written)) fail(`${file}: rendered file lacks the Koinon settings`);
}
