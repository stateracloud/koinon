// SPDX-License-Identifier: Apache-2.0

import type { ComponentProps } from 'react'
import { Textarea } from '@/components/ui/textarea'

/**
 * SecretTextarea is the secure input of ADR 0021 for multi-line secrets
 * such as a PEM private key. It opts out of autocomplete, spell checking
 * (which can send text to a cloud service) and password-manager capture, and
 * masks the text where the browser supports it. The owner clears its value
 * after submit; the API never returns the secret, so it is never shown again.
 */
export function SecretTextarea(props: Omit<ComponentProps<'textarea'>, 'autoComplete' | 'spellCheck'>) {
  return (
    <Textarea
      {...props}
      autoComplete="off"
      autoCorrect="off"
      autoCapitalize="off"
      spellCheck={false}
      data-1p-ignore
      data-lpignore="true"
      data-secret="true"
      className="font-mono text-xs [-webkit-text-security:disc]"
    />
  )
}
