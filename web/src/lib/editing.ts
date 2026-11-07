// SPDX-License-Identifier: Apache-2.0

import type { Dispatch, SetStateAction } from 'react'

function shallowEqual(a: object, b: object): boolean {
  const ka = Object.keys(a)
  return ka.length === Object.keys(b).length && ka.every((k) => (a as Record<string, unknown>)[k] === (b as Record<string, unknown>)[k])
}

// Objects match field by field (a snapshot keeps its identity, a literal does not); ids and names match by value.
function same<T>(a: T, b: T): boolean {
  return typeof a === 'object' && a !== null && typeof b === 'object' && b !== null ? shallowEqual(a, b) : Object.is(a, b)
}

/**
 * closeOnly returns a done callback that closes the form described by mine and nothing else.
 *
 * A form calls done after its save and the refetch behind it finish, which can be well after the
 * person opened another form. Clearing the page's editing state unconditionally would close that
 * other form (old issue 520), so the callback clears it only while it still names mine.
 */
export function closeOnly<T>(setEditing: Dispatch<SetStateAction<T | undefined>>, mine: T): () => void {
  return () => setEditing((cur) => (cur !== undefined && same(cur, mine) ? undefined : cur))
}
