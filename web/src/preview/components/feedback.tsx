// SPDX-License-Identifier: Apache-2.0

import { CircleCheck, CircleX, Info, TriangleAlert } from 'lucide-react'
import { useState, type ComponentProps, type FormEvent, type KeyboardEvent, type ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { Button } from '@/preview/components/button'
import { PreviewLink } from '@/preview/lib/link'
import type { Notice } from '@/preview/mock/data'

export type AlertVariant = 'default' | 'destructive' | 'warning' | 'success' | 'info'

const alertIcons: Record<AlertVariant, ReactNode> = {
  default: null,
  destructive: <CircleX aria-hidden="true" />,
  warning: <TriangleAlert aria-hidden="true" />,
  success: <CircleCheck aria-hidden="true" />,
  info: <Info aria-hidden="true" />,
}

/**
 * Alert is Koinon.Alert: a card with a coloured edge and icon. Show errors inline as a destructive Alert, never a
 * toast. role defaults to "status" ("alert" for destructive); pass icon={false} to drop the icon.
 */
export function Alert({ variant = 'default', icon = true, role, className, children, ...props }: ComponentProps<'div'> & { variant?: AlertVariant; icon?: boolean }) {
  return (
    <div role={role ?? (variant === 'destructive' ? 'alert' : 'status')} className={cn('kn-alert', variant !== 'default' && `kn-alert-${variant}`, className)} {...props}>
      {icon && alertIcons[variant]}
      {children}
    </div>
  )
}

export function AlertTitle({ className, ...props }: ComponentProps<'div'>) {
  return <div className={cn('kn-alert-title', className)} {...props} />
}

export function AlertDescription({ className, ...props }: ComponentProps<'div'>) {
  return <div className={cn('kn-alert-description', className)} {...props} />
}

/** NoticeBanner is Koinon.NoticeBanner: budget and health notices under the top bar, at most three. */
export function NoticeBanner({ notices }: { notices: Notice[] }) {
  if (notices.length === 0) return null
  return (
    <div className="kn-notices" role="region" aria-label="Notices">
      {notices.slice(0, 3).map((n) => (
        <Alert key={n.id} variant={n.urgent ? 'destructive' : 'warning'} role={n.urgent ? 'alert' : 'status'}>
          <AlertDescription style={{ color: 'var(--foreground)', display: 'block' }}>
            {n.text}{' '}
            <PreviewLink className="ks-link" href={n.to}>
              {n.linkText}
            </PreviewLink>
          </AlertDescription>
        </Alert>
      ))}
    </div>
  )
}

/**
 * EmptyState is Koinon.EmptyState: one calm sentence, centred, with an optional action. (The design system's Tess
 * illustration and soft cloud are not in the preview yet.)
 */
export function EmptyState({ title, children, action, className }: { title: string; children?: ReactNode; action?: ReactNode; className?: string }) {
  return (
    <div className={cn('kn-empty', className)}>
      <h2 className="ks-h3">{title}</h2>
      {children && <div className="ks-muted" style={{ maxWidth: '48ch' }}>{children}</div>}
      {action}
    </div>
  )
}

export type ComposerProps = {
  /** Called with the trimmed text; the box clears afterwards. */
  onSend: (text: string) => void
  idPrefix?: string
  /** The textarea's label; "Message" by default. */
  label?: string
  /** The form's accessible name; "Reply" by default. */
  formLabel?: string
  submitLabel?: string
  placeholder?: string
  /** Quiet text left of the button; "Ctrl or ⌘ + Enter sends." by default. */
  hint?: ReactNode
  disabled?: boolean
  /** Hides the visible label (it stays for screen readers). */
  hideLabel?: boolean
}

/** Composer is Koinon.Composer: the reply box (steer a run, message Daimon). Ctrl or ⌘ + Enter sends. */
export function Composer({ onSend, idPrefix = 'composer', label = 'Message', formLabel = 'Reply', submitLabel = 'Send', placeholder, hint = 'Ctrl or ⌘ + Enter sends.', disabled, hideLabel }: ComposerProps) {
  const [text, setText] = useState('')
  function submit() {
    const msg = text.trim()
    if (!msg) return
    onSend(msg)
    setText('')
  }
  function onSubmit(e: FormEvent) {
    e.preventDefault()
    submit()
  }
  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      submit()
    }
  }
  return (
    <form onSubmit={onSubmit} className="ks-stack" aria-label={formLabel}>
      <label htmlFor={idPrefix} className={hideLabel ? 'ks-sr' : 'kn-label'}>
        {label}
      </label>
      <textarea id={idPrefix} className="kn-field kn-textarea" rows={3} value={text} placeholder={placeholder} disabled={disabled} onChange={(e) => setText(e.target.value)} onKeyDown={onKeyDown} aria-describedby={`${idPrefix}-hint`} />
      <div className="kn-composer-foot">
        <span id={`${idPrefix}-hint`} className="ks-meta">
          {hint}
        </span>
        <Button type="submit" className="kn-ml-auto" disabled={disabled || !text.trim()}>
          {submitLabel}
        </Button>
      </div>
    </form>
  )
}
