// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'

/** useDebounced returns value once it has stopped changing for ms. */
export function useDebounced<T>(value: T, ms = 250): T {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setSettled(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return settled
}
