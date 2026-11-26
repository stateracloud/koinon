// SPDX-License-Identifier: Apache-2.0
// Added from the shadcn/ui registry (new-york style, "switch"); the source is ours to edit.

import * as React from "react"
import { Switch as SwitchPrimitive } from "radix-ui"
import { cn } from "@/lib/utils"

/** Switch is an on/off control (role=switch). Off has a control border, so the track keeps 3:1 (WCAG 1.4.11). */
function Switch({ className, ...props }: React.ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        "peer data-[state=checked]:bg-primary data-[state=unchecked]:bg-card border-input focus-visible:ring-ring/50 inline-flex h-5 w-9 shrink-0 items-center rounded-full border transition-all outline-none focus-visible:ring-[3px] disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:border-transparent",
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className="data-[state=checked]:bg-primary-foreground data-[state=unchecked]:bg-muted-foreground pointer-events-none block size-3.5 rounded-full ring-0 transition-transform data-[state=checked]:translate-x-[calc(100%+3px)] data-[state=unchecked]:translate-x-0.5"
      />
    </SwitchPrimitive.Root>
  )
}

export { Switch }
