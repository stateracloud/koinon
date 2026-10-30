// SPDX-License-Identifier: Apache-2.0

import { useRef, type ComponentProps, type KeyboardEvent, type ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { meterLevel } from '@/preview/lib/format'

/** PageHeader is the h1 row of a page: title, an optional line under it, and actions on the right. */
export function PageHeader({ title, description, actions, badges, className }: { title: ReactNode; description?: ReactNode; actions?: ReactNode; badges?: ReactNode; className?: string }) {
  return (
    <div className={cn('ks-head', className)}>
      <div className="ks-stack" style={{ gap: 4 }}>
        {badges ? (
          <div className="ks-row">
            <h1 className="ks-h1">{title}</h1>
            {badges}
          </div>
        ) : (
          <h1 className="ks-h1">{title}</h1>
        )}
        {description && <p className="ks-p ks-muted ks-num">{description}</p>}
      </div>
      {actions && <div className="ks-row">{actions}</div>}
    </div>
  )
}

/** Section is a titled block of a page (h2, with metadata and a link or action on the right). */
export function Section({ id, title, meta, action, children, className, panel }: { id: string; title: ReactNode; meta?: ReactNode; action?: ReactNode; children: ReactNode; className?: string; panel?: boolean }) {
  return (
    <section aria-labelledby={`${id}-h`} className={cn(panel ? 'ks-panel ks-stack-3' : 'ks-stack-3', className)}>
      <div className="ks-row" style={{ alignItems: 'baseline' }}>
        <h2 id={`${id}-h`} className={panel ? 'ks-h4' : 'ks-h3'}>
          {title}
        </h2>
        {meta && <span className="ks-meta ks-num">{meta}</span>}
        {action && <span style={{ marginLeft: 'auto' }}>{action}</span>}
      </div>
      {children}
    </section>
  )
}

/** Panel is a card surface (ks-panel); flush drops the padding for tables and lists. */
export function Panel({ flush, className, ...props }: ComponentProps<'div'> & { flush?: boolean }) {
  return <div className={cn(flush ? 'ks-panel-flush' : 'ks-panel', className)} {...props} />
}

/**
 * Table is the ks-table in a flush panel that scrolls sideways on narrow screens. Write thead and tbody as
 * children; th get scope="col"; right-aligned number cells use className="ks-r". aria-selected on a tr highlights it.
 */
export function Table({ className, wrapClassName, label, ...props }: ComponentProps<'table'> & { wrapClassName?: string; label?: string }) {
  return (
    <div className={cn('ks-panel-flush ks-table-wrap', wrapClassName)} role={label ? 'region' : undefined} aria-label={label} tabIndex={label ? 0 : undefined}>
      <table className={cn('ks-table', className)} {...props} />
    </div>
  )
}

/** Meter is a spend or usage bar: neutral fill, amber from 75%, red at the limit. label names it for screen readers. */
export function Meter({ used, limit, label, large, className }: { used: number; limit: number; label?: string; large?: boolean; className?: string }) {
  const percent = limit <= 0 ? 0 : Math.round((used / limit) * 100)
  return (
    <div className={cn('ks-meter', large && 'ks-meter-lg', className)} role="img" aria-label={label ?? `${percent}% used`}>
      <span data-level={meterLevel(percent)} style={{ width: `${Math.min(percent, 100)}%` }} />
    </div>
  )
}

/** Kbd is one key cap. */
export function Kbd({ children, className }: { children: ReactNode; className?: string }) {
  return <kbd className={cn('ks-kbd', className)}>{children}</kbd>
}

/** Keys is a sequence of key caps, such as ['G', 'I']. */
export function Keys({ keys }: { keys: string[] }) {
  return (
    <span className="ks-row" style={{ gap: 4, flexWrap: 'nowrap' }}>
      {keys.map((k, i) => (
        <Kbd key={i}>{k}</Kbd>
      ))}
    </span>
  )
}

export type TabItem<T extends string> = { id: T; label: ReactNode; count?: number }

/**
 * Tabs is the underlined tab row (ks-tabs) with arrow-key movement. Render the panel yourself below it, with
 * id={`${idPrefix}-${tab}-panel`} and role="tabpanel" (TabPanel does this).
 */
export function Tabs<T extends string>({ label, tabs, value, onChange, idPrefix = 'tabs', className }: { label: string; tabs: TabItem<T>[]; value: T; onChange: (id: T) => void; idPrefix?: string; className?: string }) {
  const refs = useRef<(HTMLButtonElement | null)[]>([])
  function onKeyDown(e: KeyboardEvent, i: number) {
    let next = -1
    if (e.key === 'ArrowRight') next = (i + 1) % tabs.length
    else if (e.key === 'ArrowLeft') next = (i - 1 + tabs.length) % tabs.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = tabs.length - 1
    if (next < 0) return
    e.preventDefault()
    onChange(tabs[next].id)
    refs.current[next]?.focus()
  }
  return (
    <div className={cn('ks-tabs', className)} role="tablist" aria-label={label}>
      {tabs.map((t, i) => (
        <button
          key={t.id}
          ref={(el) => {
            refs.current[i] = el
          }}
          type="button"
          role="tab"
          id={`${idPrefix}-${t.id}-tab`}
          aria-controls={`${idPrefix}-${t.id}-panel`}
          aria-selected={t.id === value}
          tabIndex={t.id === value ? 0 : -1}
          className="ks-tab"
          onClick={() => onChange(t.id)}
          onKeyDown={(e) => onKeyDown(e, i)}
        >
          {t.label}
          {t.count !== undefined && (
            <span className="ks-muted ks-num" style={{ fontWeight: 400 }}>
              {t.count}
            </span>
          )}
        </button>
      ))}
    </div>
  )
}

/** TabPanel is the panel of the selected tab of a Tabs row with the same idPrefix. */
export function TabPanel({ idPrefix = 'tabs', tab, className, ...props }: ComponentProps<'div'> & { idPrefix?: string; tab: string }) {
  return <div role="tabpanel" id={`${idPrefix}-${tab}-panel`} aria-labelledby={`${idPrefix}-${tab}-tab`} className={className} {...props} />
}

/** Segmented is the ks-seg toggle group (aria-pressed buttons), for views and scopes such as "Whole org / One agent". */
export function Segmented<T extends string>({ label, options, value, onChange, className }: { label: string; options: { id: T; label: ReactNode }[]; value: T; onChange: (id: T) => void; className?: string }) {
  return (
    <div className={cn('ks-seg', className)} role="group" aria-label={label}>
      {options.map((o) => (
        <button key={o.id} type="button" aria-pressed={o.id === value} onClick={() => onChange(o.id)}>
          {o.label}
        </button>
      ))}
    </div>
  )
}

export type FilterChip = { key: string; value: string; onRemove?: () => void }

/**
 * FilterBar is the typeable filter with chips (ks-filter). The input carries data-kp-filter, so "/" focuses it from
 * anywhere on the page.
 */
export function FilterBar({ id, label, chips = [], placeholder, value, onChange, className }: { id: string; label: string; chips?: FilterChip[]; placeholder?: string; value?: string; onChange?: (v: string) => void; className?: string }) {
  return (
    <label className={cn('ks-filter', className)} htmlFor={id}>
      <span className="ks-sr">{label}</span>
      {chips.map((c) => (
        <span className="ks-chip" key={c.key + c.value}>
          <b>{c.key}:</b>
          {c.value}
          <button type="button" aria-label={`Remove ${c.key} filter`} onClick={c.onRemove}>
            ×
          </button>
        </span>
      ))}
      <input id={id} type="text" data-kp-filter="" placeholder={placeholder} value={value} onChange={onChange ? (e) => onChange(e.target.value) : undefined} />
    </label>
  )
}

/** Pill is a saved view or category toggle (ks-pill with aria-pressed). */
export function Pill({ pressed, className, ...props }: ComponentProps<'button'> & { pressed?: boolean }) {
  return <button type="button" className={cn('ks-pill', className)} aria-pressed={pressed} {...props} />
}

/** CodeBlock is preformatted mono text on the code ground (requests, responses, CLI lines). */
export function CodeBlock({ className, ...props }: ComponentProps<'pre'>) {
  return <pre className={cn('ks-code', className)} {...props} />
}
