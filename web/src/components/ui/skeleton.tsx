// SPDX-License-Identifier: Apache-2.0
// Added from the shadcn/ui registry (new-york style, "skeleton"); the source is ours to edit.

import * as React from "react"
import { cn } from "@/lib/utils"

/** Skeleton is a placeholder shape. It is decoration: the loading state is said in text by its container. */
function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return <div data-slot="skeleton" aria-hidden className={cn("bg-raised animate-pulse rounded-md motion-reduce:animate-none", className)} {...props} />
}

/** SkeletonRows is the loading state of a list or table: a status line for screen readers and a few bars. */
function SkeletonRows({ label, rows = 3, className }: { label: string; rows?: number; className?: string }) {
  return (
    <div role="status" data-slot="skeleton-rows" className={cn("grid gap-2", className)}>
      <span className="sr-only">{label}</span>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  )
}

export { Skeleton, SkeletonRows }
