// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { isApprover } from '@/lib/approvals'
import { useOrg } from '@/lib/org-context'

/**
 * RequireAdmin renders its children for org admins and owners only. Members get a plain notice and the
 * children never mount, so no request goes out; the server refuses members as well.
 */
export function RequireAdmin({ what, children }: { what: string; children: ReactNode }) {
  const { org } = useOrg()
  if (!isApprover(org.role)) {
    return (
      <Alert role="alert">
        <AlertDescription>Only org admins and owners can manage {what}.</AlertDescription>
      </Alert>
    )
  }
  return <>{children}</>
}
