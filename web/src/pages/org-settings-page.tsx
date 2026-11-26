// SPDX-License-Identifier: Apache-2.0

import { ApproverNotRequesterSettings } from '@/components/approver-not-requester-settings'
import { HomeBackupRetentionSettings } from '@/components/home-backup-retention-settings'
import { OrgContinuationCap } from '@/components/org-continuation-cap'
import { OrgDefaultModel } from '@/components/org-default-model'
import { OrgMembers } from '@/components/org-members'
import { OrgRunLimits } from '@/components/org-run-limits'
import { RequireAdmin } from '@/components/require-admin'
import { RunContentRetentionSettings } from '@/components/run-content-retention-settings'
import { Card, CardContent } from '@/components/ui/card'
import { PageHeader } from '@/components/ui/page-header'
import { orgRoleLabel } from '@/lib/labels'
import { orgName, useOrg } from '@/lib/org-context'

/**
 * OrgSettingsPage is the "Org settings" item of the sidebar (ADR 0126 section 4, issue #1279): every setting of the
 * org in one place. The sections are the default model, the approver rule, the run content and home backup retention,
 * the continuation cap, the run limits (#492) and the read-only members list. Admins and owners only; each change is
 * audited by the service behind it. Org rename and identity settings are not here (they wait for the org lifecycle
 * decision).
 */
export function OrgSettingsPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <PageHeader title="Org settings" description="The settings that apply to every agent and every person in this org." />
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
        <OrgDefaultModel orgId={orgId} />
        <ApproverNotRequesterSettings orgId={orgId} />
        <RunContentRetentionSettings orgId={orgId} />
        <HomeBackupRetentionSettings orgId={orgId} />
        <OrgContinuationCap orgId={orgId} />
        <OrgRunLimits orgId={orgId} />
        <OrgMembers orgId={orgId} />
      </RequireAdmin>
    </div>
  )
}
