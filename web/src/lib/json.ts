// SPDX-License-Identifier: Apache-2.0

// Guards for JSON from the API (google.protobuf.Struct payloads, connector
// documents): a value of an unexpected type reads as absent.

import type { JsonObject, JsonValue } from '@bufbuild/protobuf'

/** obj is v if it is a JSON object (not an array or null). */
export function obj(v: JsonValue | undefined): JsonObject | undefined {
  return v && typeof v === 'object' && !Array.isArray(v) ? v : undefined
}

/** arr is v if it is an array, else empty. */
export function arr(v: JsonValue | undefined): JsonValue[] {
  return Array.isArray(v) ? v : []
}

/** str is v if it is a string. */
export function str(v: JsonValue | undefined): string | undefined {
  return typeof v === 'string' ? v : undefined
}
