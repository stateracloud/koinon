// SPDX-License-Identifier: Apache-2.0

import type { ComponentProps, ReactNode } from 'react'
import { cn } from '@/lib/utils'

/** Label is Koinon.Label: above each field. */
export function Label({ className, ...props }: ComponentProps<'label'>) {
  return <label className={cn('kn-label', className)} {...props} />
}

export function Input({ className, type, ...props }: ComponentProps<'input'>) {
  return <input type={type ?? 'text'} className={cn('kn-field kn-input', className)} {...props} />
}

export function Textarea({ className, ...props }: ComponentProps<'textarea'>) {
  return <textarea className={cn('kn-field kn-textarea', className)} {...props} />
}

/** NativeSelect is the select for forms (the design system keeps the custom Select for the org switcher only). */
export function NativeSelect({ className, ...props }: ComponentProps<'select'>) {
  return <select className={cn('kn-field kn-native-select', className)} {...props} />
}

/**
 * Field is a Label above a control with an optional hint and error under it. Give the control id={id}; the hint and
 * error are wired up with aria-describedby by passing describedBy(id) to the control.
 */
export function Field({ id, label, hint, error, children, className }: { id: string; label: ReactNode; hint?: ReactNode; error?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <div className={cn('ks-stack', className)} style={{ gap: 6 }}>
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint && (
        <p id={`${id}-hint`} className="ks-p ks-meta">
          {hint}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} className="ks-p ks-err" style={{ fontSize: 13 }}>
          {error}
        </p>
      )}
    </div>
  )
}

/**
 * TypeToConfirm is the "type the name to confirm" field of destructive dialogs (delete an org, resume over a
 * limit). Enable the destructive button when value === expected.
 */
export function TypeToConfirm({ id, expected, value, onChange, label }: { id: string; expected: string; value: string; onChange: (v: string) => void; label?: ReactNode }) {
  return (
    <Field id={id} label={label ?? <span>Type <span className="ks-mono">{expected}</span> to confirm</span>}>
      <Input id={id} value={value} onChange={(e) => onChange(e.target.value)} autoComplete="off" spellCheck={false} aria-invalid={value !== '' && !expected.startsWith(value) ? true : undefined} />
    </Field>
  )
}
