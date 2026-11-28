// SPDX-License-Identifier: Apache-2.0

// The toast store (issue #1270). <Toaster> (components/ui/toast.tsx) renders it.
//
// The feedback pattern: a toast says that an action worked. A failure is never a toast: it is an inline error
// next to the action (components/ui/inline-error.tsx, hooks/use-feedback.ts), because a toast goes away and the
// person must read the error and act on it. A toast holds no link and no control the person needs: it is gone
// after a few seconds.

export type ToastItem = { id: number; message: string }

type Listener = () => void

let items: ToastItem[] = []
let nextId = 1
const listeners = new Set<Listener>()

function emit() {
  for (const l of listeners) l()
}

export function dismissToast(id: number) {
  items = items.filter((t) => t.id !== id)
  emit()
}

/** toast shows a success message. Call it after the action worked, with a short sentence ("Token revoked."). */
export function toast(message: string): number {
  const id = nextId++
  // At most three at once: the oldest goes first.
  items = [...items, { id, message }].slice(-3)
  emit()
  return id
}

export function subscribeToasts(l: Listener) {
  listeners.add(l)
  return () => {
    listeners.delete(l)
  }
}

export function toastSnapshot() {
  return items
}

/** resetToasts clears every toast. For tests only. */
export function resetToasts() {
  items = []
  emit()
}
