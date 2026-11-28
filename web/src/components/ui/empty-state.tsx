// SPDX-License-Identifier: Apache-2.0

import * as React from "react"
import { cn } from "@/lib/utils"

export type EmptyStateProps = {
  /** A decorative lucide icon (it is hidden from screen readers). */
  icon?: React.ComponentType<{ className?: string; "aria-hidden"?: boolean }>
  /** What is empty, in a few words: "No agents yet". */
  title: string
  /** What the person can do next. An empty state always names the next step (ui-foundations section 9). */
  description?: React.ReactNode
  /** The action that fills the list, for example a "New agent" button. */
  action?: React.ReactNode
  className?: string
}

/** EmptyState shows in place of a list or table that has no rows. */
export function EmptyState({ icon: Icon, title, description, action, className }: EmptyStateProps) {
  return (
    <div data-slot="empty-state" className={cn("bg-card grid justify-items-center gap-2 rounded-lg border border-dashed p-8 text-center", className)}>
      {Icon && <Icon aria-hidden className="text-muted-foreground size-6" />}
      <p className="font-medium">{title}</p>
      {description && <div className="text-muted-foreground max-w-prose text-sm">{description}</div>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}
