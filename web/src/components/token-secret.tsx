// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'

/**
 * TokenSecret shows a new API token once. It lives in this component's state only: dismissing it, or
 * leaving the page, drops it, and Koinon cannot show it again because it keeps only a hash.
 */
export function TokenSecret({ name, secret, onDismiss }: { name: string; secret: string; onDismiss: () => void }) {
  const [copied, setCopied] = useState<'yes' | 'no'>()

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret)
      setCopied('yes')
    } catch {
      setCopied('no')
    }
  }

  return (
    <Alert role="region" aria-label="New API token" className="grid gap-3">
      <AlertTitle>Token {name} created</AlertTitle>
      <AlertDescription className="grid gap-3">
        <p className="text-foreground font-medium">Copy it now. You won&apos;t see it again.</p>
        <div className="flex flex-wrap items-center gap-2">
          <code className="bg-muted text-foreground rounded px-2 py-1 text-xs break-all" data-testid="token-secret">
            {secret}
          </code>
          <Button size="sm" onClick={() => void copy()}>
            Copy token
          </Button>
        </div>
        {copied === 'yes' && <p role="status">Copied to the clipboard.</p>}
        {copied === 'no' && <p role="status">Your browser would not copy it. Select the token above and copy it by hand.</p>}
        <p className="text-muted-foreground text-xs">
          Send it as <code>Authorization: Bearer &lt;token&gt;</code> to <code>/admin/v1</code>. Koinon stores only a hash of it.
        </p>
        <div>
          <Button variant="outline" size="sm" onClick={onDismiss}>
            I have copied it
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  )
}
