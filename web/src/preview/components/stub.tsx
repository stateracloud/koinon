// SPDX-License-Identifier: Apache-2.0

import { Badge } from '@/preview/components/button'
import { PageHeader } from '@/preview/components/layout'
import { PreviewLink } from '@/preview/lib/link'
import { PreviewShell, type Crumb } from '@/preview/shell/shell'
import type { Space } from '@/preview/shell/nav'

/**
 * ScreenStub is the placeholder of a screen still to be built: it sits in the real shell (sidebar, top bar,
 * palette) so the flow between screens already works. Replace it with the screen.
 */
export function ScreenStub({ title, mockups, space = 'org', current, crumbs, file }: { title: string; mockups: string[]; space?: Space; current?: string; crumbs: Crumb[]; file: string }) {
  return (
    <PreviewShell space={space} current={current} crumbs={crumbs}>
      <PageHeader title={title} badges={<Badge variant="outline">To be built</Badge>} />
      <div className="kp-stub">
        <p className="ks-p">
          This screen is built from <span className="ks-mono">{mockups.join(', ')}</span> in <span className="ks-mono">{file}</span>.
        </p>
        <p className="ks-p ks-muted">
          The shell around it already works: switch spaces, press ⌘K or ?, or go back to{' '}
          <PreviewLink className="ks-link" href="/preview">
            every screen
          </PreviewLink>
          .
        </p>
      </div>
    </PreviewShell>
  )
}
