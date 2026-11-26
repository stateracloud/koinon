// SPDX-License-Identifier: Apache-2.0
// Added from the shadcn/ui registry (new-york style, "table"); the source is ours to edit.

import * as React from "react"
import { cn } from "@/lib/utils"

/** Table is a plain HTML table in a region that scrolls on its own on a narrow screen (WCAG 1.4.10 Reflow). */
function Table({ className, regionLabel, ...props }: React.ComponentProps<"table"> & { regionLabel?: string }) {
  return (
    <div
      data-slot="table-container"
      className="relative w-full overflow-x-auto"
      // A scroll region must be reachable by keyboard and have a name (axe scrollable-region-focusable).
      tabIndex={regionLabel ? 0 : undefined}
      role={regionLabel ? "region" : undefined}
      aria-label={regionLabel}
    >
      <table data-slot="table" className={cn("w-full caption-bottom text-left text-sm", className)} {...props} />
    </div>
  )
}

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return <thead data-slot="table-header" className={cn("[&_tr]:border-b", className)} {...props} />
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return <tbody data-slot="table-body" className={cn("[&_tr:last-child]:border-0", className)} {...props} />
}

function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return <tr data-slot="table-row" className={cn("border-b align-top", className)} {...props} />
}

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return <th data-slot="table-head" className={cn("text-muted-foreground py-2 pr-3 font-medium whitespace-nowrap last:pr-0", className)} {...props} />
}

function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return <td data-slot="table-cell" className={cn("py-2 pr-3 last:pr-0", className)} {...props} />
}

function TableCaption({ className, ...props }: React.ComponentProps<"caption">) {
  return <caption data-slot="table-caption" className={cn("text-muted-foreground mt-4 text-sm", className)} {...props} />
}

export { Table, TableHeader, TableBody, TableRow, TableHead, TableCell, TableCaption }
