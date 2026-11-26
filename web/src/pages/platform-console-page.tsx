// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { OpenOrgButton } from '@/components/open-org-button'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { useAllOrgs } from '@/hooks/use-all-orgs'
import { useOrgSession } from '@/lib/org-context'
import { orgSummaryName, roleText, spendText } from '@/lib/platform-orgs'

/** homeOrgs is how many orgs the console home shows; the Orgs page shows all of them. */
const homeOrgs = 5

/**
 * PlatformConsolePage is the home of the platform console at /platform (ADR 0126 section 4). It shows the first orgs
 * by name and a link to all of them. The platform totals come with issue #1281.
 */
export function PlatformConsolePage() {
  const { memberships } = useOrgSession()
  const list = useAllOrgs()
  const orgs = [...(list.orgs ?? [])].sort((a, b) => orgSummaryName(a).localeCompare(orgSummaryName(b))).slice(0, homeOrgs)
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <PageHeader title="Platform console" description="Settings for all the orgs of this Koinon." />
      <section aria-labelledby="home-orgs" className="grid gap-3">
        <div className="flex items-baseline justify-between gap-3">
          <h2 id="home-orgs" className="text-lg font-semibold">
            Orgs
          </h2>
          <Link to="/platform/orgs" className="text-link text-sm font-medium hover:underline">
            View all orgs
          </Link>
        </div>
        <QueryError error={list.error ?? undefined} />
        {list.isPending ? (
          <SkeletonRows label="Loading orgs…" />
        ) : orgs.length === 0 ? (
          !list.error && <p className="text-muted-foreground text-sm">No orgs yet.</p>
        ) : (
          <ul aria-labelledby="home-orgs" className="grid gap-2">
            {orgs.map((o) => (
              <li key={o.orgId} className="bg-card flex flex-wrap items-center gap-3 rounded-lg border p-3">
                <span className="grid min-w-0 flex-1">
                  <span className="truncate font-medium">{orgSummaryName(o)}</span>
                  <span className="text-muted-foreground text-xs">
                    {roleText(o, memberships)} · {spendText(o)}
                  </span>
                </span>
                <OpenOrgButton org={o} />
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}
