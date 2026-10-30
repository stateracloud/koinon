// SPDX-License-Identifier: Apache-2.0

import { forwardRef, type AnchorHTMLAttributes, type MouseEvent } from 'react'
import { usePathname, usePreviewNavigate } from '@/preview/lib/navigation'

export type PreviewLinkProps = Omit<AnchorHTMLAttributes<HTMLAnchorElement>, 'href'> & {
  href: string
  /** Sets aria-current="page" when the current path is href (or starts with it, with prefix). */
  activeMatch?: 'exact' | 'prefix'
}

/**
 * PreviewLink is a real link (it opens in a new tab, copies and middle-clicks like one) that navigates client side
 * on a plain click. Use it for every link between preview screens.
 */
export const PreviewLink = forwardRef<HTMLAnchorElement, PreviewLinkProps>(function PreviewLink(
  { href, activeMatch, onClick, ...props },
  ref,
) {
  const navigate = usePreviewNavigate()
  const pathname = usePathname()
  const path = href.split(/[?#]/)[0]
  const active = activeMatch === 'exact' ? pathname === path : activeMatch === 'prefix' ? pathname === path || pathname.startsWith(path + '/') : false
  function handle(e: MouseEvent<HTMLAnchorElement>) {
    onClick?.(e)
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    if (href.startsWith('#')) return
    e.preventDefault()
    navigate(href)
  }
  return <a ref={ref} href={href} {...props} aria-current={active ? 'page' : props['aria-current']} onClick={handle} />
})
