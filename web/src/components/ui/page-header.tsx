// SPDX-License-Identifier: Apache-2.0

import * as React from "react"
import { cn } from "@/lib/utils"

export type PageHeaderProps = {
  /** The page's one h1 (S1 point 9 of docs/research/ui-foundations.md). */
  title: React.ReactNode
  /** One or two sentences under the title: what the page is for. */
  description?: React.ReactNode
  /** A <Breadcrumb> above the title, on a page under another page. */
  breadcrumb?: React.ReactNode
  /** The primary action (and at most one or two more), at the top right. */
  actions?: React.ReactNode
  className?: string
}

/** PageHeader is the top of every page: an optional breadcrumb, the h1, the description and the primary action. */
export function PageHeader({ title, description, breadcrumb, actions, className }: PageHeaderProps) {
  return (
    <header data-slot="page-header" className={cn("grid gap-2", className)}>
      {breadcrumb}
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{title}</h1>
        {actions && <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {description && <div className="text-muted-foreground text-sm">{description}</div>}
    </header>
  )
}
