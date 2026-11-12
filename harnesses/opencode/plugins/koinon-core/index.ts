// SPDX-License-Identifier: Apache-2.0
//
// koinon.core: Koinon's one owned OpenCode plugin (ADR 0093). This first slice only registers the
// load report command, `koinon-core-ready-v1`; later slices (ADR 0093 decision 3.2-3.5) add
// modules to the same `setup`, each registered and awaited before the load report, so a module
// that fails to register still fails the whole plugin's load (ADR 0093 decision 2: advisory, but a
// broken module must never look loaded).
//
// Loading (ADR 0093 decision 4; verified against OpenCode v2.2.6,
// https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06):
// the config entry names this directory by absolute path; OpenCode's `Host.resolve`
// (packages/plugin/src/host.ts#L25-L69) tries `<dir>/server` then `<dir>/index` through Bun's own
// module resolution, so this file must stay the only root module (`index.ts`); a later module goes
// in a subdirectory. Only `import type` is used from `@opencode/plugin` (packages/plugin/src/
// promise/plugin.ts#L27-L71): the import is erased at compile time, so the plugin has no run-time
// npm dependency (ADR 0093 decision 2 and 4).
//
// Setup failure (packages/core/src/plugin.ts#L62-L100, packages/plugin/src/promise/adapter.ts#
// L666-L669): `setup` runs inside `Effect.promise`, so a throw (from option validation, or a hook
// this pin lacks) is caught per plugin, closes this plugin's scope (nothing it registered stays
// up) and is logged; other plugins keep running. That is this slice's load report: a broken
// koinon.core never reaches the command registration below, so `koinon-core-ready-v1` never
// appears and agentd's wait fails the start with its own reason (ADR 0093 decision 3.1; the agentd
// side is out of this PR, tracked in the ADR's Delivery step 4).
//
// Hook callbacks must never throw (packages/plugin/src/promise/adapter.ts#L308,#L481,#L625,
// #L660-L662): every non-tool hook is wrapped so a throw becomes an OpenCode-side defect with
// unknown effect on the turn, not a report. Later modules follow this rule too.
import type { Plugin } from "@opencode/plugin"

/** The load report command name. The contract version is in the name (ADR 0093 decision 3.1):
 * agentd keeps only command names from ACP, never descriptions. */
export const READY_COMMAND = "koinon-core-ready-v1"

/** The plugin's own id, reserved for Koinon (ADR 0093 decision 4: `koinon.*` plugin IDs). */
export const PLUGIN_ID = "koinon.core"

/** validateOptions rejects anything koinon.core does not expect: an unknown key, or `options` that
 * is not a plain object (a Map or a Date included). This slice declares no option, so any key is unknown and any non-object
 * throws. A later module adds its keys here, typed, so a future misspelled option fails the start
 * instead of being silently ignored (ADR 0093 decision 2, "Recommendations" 5). Thrown messages are
 * prefixed "koinon.core: " to match `koinon.retry`'s convention
 * (harnesses/opencode/plugins/koinon-retry/index.js). */
export function validateOptions(options: unknown): Readonly<Record<string, never>> {
  if (options === undefined) return {}
  if (options === null || typeof options !== "object" || Array.isArray(options)) {
    throw new Error(`koinon.core: options must be an object, got ${JSON.stringify(options)}`)
  }
  // Only a plain object (JSON config parses to one): a Map or a Date has no own enumerable keys,
  // so the key check below would wave it through.
  const proto: unknown = Object.getPrototypeOf(options)
  if (proto !== Object.prototype && proto !== null) {
    throw new Error(`koinon.core: options must be a plain object, got ${Object.prototype.toString.call(options)}`)
  }
  const keys = Object.keys(options)
  if (keys.length > 0) {
    throw new Error(`koinon.core: unknown option(s): ${keys.join(", ")}`)
  }
  return {}
}

const plugin = {
  id: PLUGIN_ID,
  setup: async (ctx: Plugin.Context) => {
    validateOptions(ctx.options)

    // Later modules register their hooks here (ADR 0093 decision 3.2-3.5), each awaited before the
    // load report below, so a module that fails to register keeps this plugin from reporting ready.

    // Load report, registered last (ADR 0093 decision 3.1): its presence in a session's
    // available_commands_update is agentd's proof that koinon.core, and everything it registered
    // above, loaded. The command's own execute is a no-op: agentd never sends it as a prompt
    // (ADR 0093, "Unverified").
    await ctx.command.transform((editor) =>
      editor.add({
        name: READY_COMMAND,
        description: "koinon.core loaded (contract v1)",
        execute: async () => {},
      }),
    )
  },
} satisfies Plugin.Plugin

export default plugin
