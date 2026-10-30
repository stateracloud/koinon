// SPDX-License-Identifier: Apache-2.0

import { Slot } from 'radix-ui'
import type { ComponentProps } from 'react'
import { cn } from '@/lib/utils'
import { buttonClass, type ButtonSize, type ButtonVariant } from '@/preview/lib/format'
import { PreviewLink, type PreviewLinkProps } from '@/preview/lib/link'

export type ButtonProps = ComponentProps<'button'> & { variant?: ButtonVariant; size?: ButtonSize; asChild?: boolean }

/** Button is Koinon.Button. Icon-only buttons need an aria-label. */
export function Button({ variant, size, asChild, className, type, ...props }: ButtonProps) {
  if (asChild) return <Slot.Root className={buttonClass(variant, size, className)} {...props} />
  return <button type={type ?? 'button'} className={buttonClass(variant, size, className)} {...props} />
}

/** ButtonLink is a link to another preview screen that looks like a button. */
export function ButtonLink({ variant, size, className, ...props }: PreviewLinkProps & { variant?: ButtonVariant; size?: ButtonSize }) {
  return <PreviewLink className={buttonClass(variant, size, className)} {...props} />
}

export type BadgeVariant = 'default' | 'secondary' | 'destructive' | 'outline' | 'ghost' | 'link'

/** Badge is Koinon.Badge: a neutral label such as "Org override" or "Coming in v0.3". */
export function Badge({ variant = 'secondary', className, ...props }: ComponentProps<'span'> & { variant?: BadgeVariant }) {
  return <span className={cn('kn-badge', `kn-badge-${variant}`, className)} {...props} />
}
