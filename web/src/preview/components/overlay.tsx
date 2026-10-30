// SPDX-License-Identifier: Apache-2.0

import { X } from 'lucide-react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { usePortal } from '@/preview/lib/context'

export type DialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  /** One line under the title; also the dialog's accessible description. */
  description?: ReactNode
  /** The footer row, usually an outline way out and one primary (or destructive) button on the right. */
  footer?: ReactNode
  size?: 'default' | 'wide'
  children?: ReactNode
  className?: string
  /** Hides the close (×) button, for steps that must be finished or canceled with the footer. */
  hideClose?: boolean
}

/**
 * Dialog is a modal on radix Dialog (focus trap, Escape, return focus), styled as the mockups' ks-dialog over the
 * scrim. It portals into the preview root so the theme tokens apply.
 */
export function Dialog({ open, onOpenChange, title, description, footer, size = 'default', children, className, hideClose }: DialogProps) {
  const container = usePortal()
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal container={container}>
        <DialogPrimitive.Overlay className="kp-scrim" />
        <DialogPrimitive.Content className={cn('ks-overlay kp-dialog', className)} data-size={size}>
          <div className="ks-stack" style={{ gap: 4, paddingRight: hideClose ? 0 : 32 }}>
            <DialogPrimitive.Title className="ks-h3">{title}</DialogPrimitive.Title>
            {description ? (
              <DialogPrimitive.Description className="ks-p" style={{ color: 'var(--popover-foreground)' }}>
                {description}
              </DialogPrimitive.Description>
            ) : (
              <DialogPrimitive.Description className="ks-sr">{typeof title === 'string' ? title : 'Dialog'}</DialogPrimitive.Description>
            )}
          </div>
          {children}
          {footer && (
            <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
              {footer}
            </div>
          )}
          {!hideClose && (
            <DialogPrimitive.Close className="kp-icon-btn kp-close" aria-label="Close">
              <X className="ks-icon" aria-hidden="true" />
            </DialogPrimitive.Close>
          )}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

/** DialogClose wraps a footer button so it closes the dialog (asChild). */
export function DialogClose({ children }: { children: ReactNode }) {
  return <DialogPrimitive.Close asChild>{children}</DialogPrimitive.Close>
}

/**
 * Drawer is a side sheet from the right (the prices drawer, a provider test result), on radix Dialog. modal=false
 * keeps the page behind it usable, as in the mockups where the table stays clickable.
 */
export function Drawer({ open, onOpenChange, title, description, footer, children, className, modal = true }: Omit<DialogProps, 'size' | 'hideClose'> & { modal?: boolean }) {
  const container = usePortal()
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange} modal={modal}>
      <DialogPrimitive.Portal container={container}>
        {modal && <DialogPrimitive.Overlay className="kp-scrim" />}
        <DialogPrimitive.Content
          className={cn('ks-overlay kp-drawer', className)}
          onInteractOutside={modal ? undefined : (e) => e.preventDefault()}
        >
          <div className="ks-stack" style={{ gap: 4, paddingRight: 32 }}>
            <DialogPrimitive.Title className="ks-h2">{title}</DialogPrimitive.Title>
            {description ? (
              <DialogPrimitive.Description className="ks-p" style={{ color: 'var(--popover-foreground)' }}>
                {description}
              </DialogPrimitive.Description>
            ) : (
              <DialogPrimitive.Description className="ks-sr">{typeof title === 'string' ? title : 'Details'}</DialogPrimitive.Description>
            )}
          </div>
          {children}
          {footer && <div className="ks-row">{footer}</div>}
          <DialogPrimitive.Close className="kp-icon-btn kp-close" aria-label="Close">
            <X className="ks-icon" aria-hidden="true" />
          </DialogPrimitive.Close>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
