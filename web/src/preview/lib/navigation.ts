// SPDX-License-Identifier: Apache-2.0

import { useRouter, useRouterState } from '@tanstack/react-router'
import { useCallback } from 'react'

/**
 * usePreviewNavigate goes to a preview path such as "/preview/runs/9c41d07a" or "/preview/overview?tab=board".
 * Preview screens link by plain path strings so a screen can point at any other screen (including ones with
 * params) without importing routes; the router still handles the change client side.
 */
export function usePreviewNavigate(): (href: string) => void {
  const router = useRouter()
  return useCallback((href: string) => router.history.push(href), [router])
}

/** usePathname is the current path, without the query. */
export function usePathname(): string {
  return useRouterState({ select: (s) => s.location.pathname })
}
