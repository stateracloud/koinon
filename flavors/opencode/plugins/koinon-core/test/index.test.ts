// SPDX-License-Identifier: Apache-2.0
//
// Unit tests for koinon.core (ADR 0093 first slice): option validation and command registration,
// against a fake OpenCode plugin context. Run with `node --test` (Node 24's native type stripping
// needs only erasable syntax, which the plugin and these tests keep to); never copied into the
// image (flavors/opencode/Dockerfile copies only index.ts).
import assert from "node:assert/strict"
import { test } from "node:test"
import type { Plugin } from "@opencode/plugin"
import plugin, { PLUGIN_ID, READY_COMMAND, validateOptions } from "../index.ts"

interface AddedCommand {
  readonly name: string
  readonly description?: string
  readonly execute: (...args: unknown[]) => Promise<void>
}

// fakeContext builds the minimal slice of Plugin.Context that setup() reads: ctx.options and
// ctx.command.transform. The cast to Plugin.Context is deliberate: a real context has many more
// domains this slice never touches, and the fake only needs to be truthful about the two it uses.
function fakeContext(options: unknown): { ctx: Plugin.Context; added: AddedCommand[] } {
  const added: AddedCommand[] = []
  const ctx = {
    options,
    command: {
      transform: async (callback: (editor: { add: (def: AddedCommand) => void }) => void) => {
        callback({
          add: (def) => {
            added.push(def)
          },
        })
        return { dispose: async () => {} }
      },
    },
  } as unknown as Plugin.Context
  return { ctx, added }
}

test("default export has id koinon.core", () => {
  assert.equal(plugin.id, PLUGIN_ID)
  assert.equal(plugin.id, "koinon.core")
})

test("setup registers exactly koinon-core-ready-v1 with no options", async () => {
  const { ctx, added } = fakeContext(undefined)
  await plugin.setup(ctx)
  assert.deepEqual(
    added.map((d) => d.name),
    [READY_COMMAND],
  )
  assert.equal(added[0]?.description, "koinon.core loaded (contract v1)")
})

test("setup registers exactly koinon-core-ready-v1 with empty options", async () => {
  const { ctx, added } = fakeContext({})
  await plugin.setup(ctx)
  assert.deepEqual(
    added.map((d) => d.name),
    [READY_COMMAND],
  )
})

test("setup throws and registers nothing on an unknown option", async () => {
  const { ctx, added } = fakeContext({ bogus: "x" })
  await assert.rejects(() => plugin.setup(ctx), /koinon\.core: unknown option\(s\): bogus/)
  assert.deepEqual(added, [])
})

test("setup throws and registers nothing when options is an array", async () => {
  const { ctx, added } = fakeContext([])
  await assert.rejects(() => plugin.setup(ctx), /koinon\.core: options must be an object/)
  assert.deepEqual(added, [])
})

test("setup throws and registers nothing when options is not an object", async () => {
  const { ctx, added } = fakeContext("nope")
  await assert.rejects(() => plugin.setup(ctx), /koinon\.core: options must be an object/)
  assert.deepEqual(added, [])
})

test("validateOptions: undefined and {} are accepted", () => {
  assert.deepEqual(validateOptions(undefined), {})
  assert.deepEqual(validateOptions({}), {})
})

test("validateOptions: null, an array, a non-object and a non-plain object throw", () => {
  assert.throws(() => validateOptions(null), /koinon\.core: options must be an object/)
  assert.throws(() => validateOptions([]), /koinon\.core: options must be an object/)
  assert.throws(() => validateOptions(42), /koinon\.core: options must be an object/)
  assert.throws(() => validateOptions("x"), /koinon\.core: options must be an object/)
  assert.throws(() => validateOptions(new Map()), /koinon\.core: options must be a plain object/)
  assert.throws(() => validateOptions(new Date(0)), /koinon\.core: options must be a plain object/)
  assert.deepEqual(validateOptions(Object.create(null)), {})
})

test("validateOptions: an unknown key throws, naming it", () => {
  assert.throws(() => validateOptions({ a: 1, b: 2 }), /koinon\.core: unknown option\(s\): a, b/)
})
