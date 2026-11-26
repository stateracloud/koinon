// SPDX-License-Identifier: Apache-2.0
// Toasts on the Radix Toast primitive (issue #1270). shadcn/ui replaced its Radix toast with a third-party
// package; Radix ships in radix-ui already, so this file keeps the dependency list as it is.

import * as React from "react"
import { CheckCircle2Icon, XIcon } from "lucide-react"
import { Toast as ToastPrimitive } from "radix-ui"
import { dismissToast, subscribeToasts, toastSnapshot } from "@/lib/toast"
import { cn } from "@/lib/utils"

/** Toaster renders the toasts. Mount it once, at the root of the app. */
export function Toaster({ duration = 5000 }: { duration?: number }) {
  const list = React.useSyncExternalStore(subscribeToasts, toastSnapshot, toastSnapshot)
  return (
    <ToastPrimitive.Provider duration={duration} label="Status message" swipeDirection="right">
      {list.map((t) => (
        <ToastPrimitive.Root
          key={t.id}
          // Background: a polite announcement. A success message does not interrupt the person.
          type="background"
          defaultOpen
          onOpenChange={(open) => {
            if (!open) dismissToast(t.id)
          }}
          data-slot="toast"
          className={cn(
            "bg-card text-card-foreground data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-80 data-[state=open]:slide-in-from-bottom-full data-[swipe=end]:animate-out data-[swipe=move]:translate-x-(--radix-toast-swipe-move-x) data-[swipe=cancel]:translate-x-0 pointer-events-auto flex w-full items-start gap-3 rounded-lg border border-input p-4 text-sm shadow-lg motion-reduce:animate-none",
          )}
        >
          <CheckCircle2Icon aria-hidden className="text-success mt-0.5 size-4 shrink-0" />
          <ToastPrimitive.Description className="flex-1">{t.message}</ToastPrimitive.Description>
          <ToastPrimitive.Close
            aria-label="Dismiss"
            className="focus-visible:ring-ring/50 rounded-xs outline-none focus-visible:ring-[3px]"
          >
            <XIcon aria-hidden className="size-4" />
          </ToastPrimitive.Close>
        </ToastPrimitive.Root>
      ))}
      <ToastPrimitive.Viewport label="Status messages ({hotkey})" className="fixed right-0 bottom-0 z-[100] flex max-h-screen w-full flex-col gap-2 p-4 outline-none sm:max-w-sm" />
    </ToastPrimitive.Provider>
  )
}
