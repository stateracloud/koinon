// SPDX-License-Identifier: Apache-2.0

// The classes of the two shells (ADR 0126 section 4): the org area (pages/app-layout.tsx) and the platform console
// (pages/platform-layout.tsx) have the same sidebar and links.

export const skipLink = 'bg-raised sr-only z-50 rounded-md px-3 py-1.5 text-sm focus:not-sr-only focus:absolute focus:top-2 focus:left-2'
export const sidebar = 'bg-card flex w-full shrink-0 flex-col gap-3 border-b p-3 md:sticky md:top-0 md:h-screen md:w-60 md:overflow-y-auto md:border-r md:border-b-0'
export const navLink = 'block rounded-md px-3 py-1.5 text-sm hover:bg-accent'
export const navActive = { 'aria-current': 'page' as const, className: 'bg-accent font-medium' }
export const navGroup = 'flex flex-wrap items-center gap-1 md:mt-3 md:grid'
export const navGroupHeading = 'text-muted-foreground px-3 text-xs font-semibold tracking-wide uppercase md:pb-1'
