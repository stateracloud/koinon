// SPDX-License-Identifier: Apache-2.0

import { RequireAdmin } from '@/components/require-admin'
import { Card, CardContent } from '@/components/ui/card'
import { PageHeader } from '@/components/ui/page-header'
import { orgRoleLabel } from '@/lib/labels'
import { orgName, useOrg } from '@/lib/org-context'

/**
 * OrgSettingsPage is the "Org settings" item of the sidebar. This first version shows what the org is. The org
 * settings sections (default model, approver rule, retention, caps, run limits, members) move here with issue #1279.
 */
export function OrgSettingsPage() {
  const { org } = useOrg()
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <PageHeader title="Org settings" description="The org that you work in." />
      <RequireAdmin what="the org settings">
        <Card>
          <CardContent>
            <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
              <dt className="text-muted-foreground">Name</dt>
              <dd>{orgName(org)}</dd>
              <dt className="text-muted-foreground">Slug</dt>
              <dd className="font-mono">{org.orgSlug}</dd>
              <dt className="text-muted-foreground">Your role</dt>
              <dd>{orgRoleLabel(org.role)}</dd>
            </dl>
          </CardContent>
        </Card>
      </RequireAdmin>
    </div>
  )
}
