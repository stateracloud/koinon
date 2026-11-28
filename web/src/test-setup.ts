// SPDX-License-Identifier: Apache-2.0

import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'
import { resetToasts } from '@/lib/toast'

afterEach(() => {
  cleanup()
  localStorage.clear()
  // The toast store is a module: without isolation (local runs) a toast of one test file shows up in the next file.
  resetToasts()
})

// Radix Select needs pointer capture and scrolling, which jsdom lacks.
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false
  Element.prototype.releasePointerCapture = () => {}
  Element.prototype.setPointerCapture = () => {}
}
if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
// TanStack Router restores scroll on navigation.
window.scrollTo = () => {}
