// SPDX-License-Identifier: Apache-2.0
// Added from the shadcn/ui registry (new-york style, "breadcrumb"); the source is ours to edit.

import * as React from "react"
import { ChevronRightIcon } from "lucide-react"
import { Slot } from "radix-ui"
import { cn } from "@/lib/utils"

function Breadcrumb(props: React.ComponentProps<"nav">) {
  return <nav aria-label="Breadcrumb" data-slot="breadcrumb" {...props} />
}

function BreadcrumbList({ className, ...props }: React.ComponentProps<"ol">) {
  return <ol data-slot="breadcrumb-list" className={cn("text-muted-foreground flex flex-wrap items-center gap-1.5 text-sm break-words", className)} {...props} />
}

function BreadcrumbItem({ className, ...props }: React.ComponentProps<"li">) {
  return <li data-slot="breadcrumb-item" className={cn("inline-flex items-center gap-1.5", className)} {...props} />
}

/** BreadcrumbLink takes `asChild` so a router Link renders it. */
function BreadcrumbLink({ asChild, className, ...props }: React.ComponentProps<"a"> & { asChild?: boolean }) {
  const Comp = asChild ? Slot.Root : "a"
  return <Comp data-slot="breadcrumb-link" className={cn("hover:text-foreground underline-offset-4 hover:underline", className)} {...props} />
}

function BreadcrumbPage({ className, ...props }: React.ComponentProps<"span">) {
  return <span data-slot="breadcrumb-page" aria-current="page" className={cn("text-foreground font-normal", className)} {...props} />
}

function BreadcrumbSeparator({ children, className, ...props }: React.ComponentProps<"li">) {
  return (
    <li data-slot="breadcrumb-separator" role="presentation" aria-hidden className={cn("[&>svg]:size-3.5", className)} {...props}>
      {children ?? <ChevronRightIcon />}
    </li>
  )
}

export { Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator }
