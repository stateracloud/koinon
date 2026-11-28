// SPDX-License-Identifier: Apache-2.0

import { useInfiniteQuery } from '@connectrpc/connect-query'
import { UsersIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { SkeletonRows } from '@/components/ui/skeleton'
import { OrgService, type Member } from '@/gen/koinon/org/v1alpha1/org_pb'
import { dataTableColumns } from '@/lib/data-table'
import { orgRoleLabel } from '@/lib/labels'

const col = dataTableColumns<Member>()
// The server sorts by display name and pages, so the table does not sort on the client.
const columns = [
  col.accessor('displayName', { header: 'Name', enableSorting: false, meta: { rowHeader: true } }),
  col.accessor((m) => orgRoleLabel(m.role), { id: 'role', header: 'Role', enableSorting: false }),
]

/**
 * OrgMembers is the read-only list of the org's members with their roles (issue #1279, the members read of #1264).
 * Changing membership is not part of the org settings page.
 */
export function OrgMembers({ orgId }: { orgId: string }) {
  const list = useInfiniteQuery(OrgService.method.listMembers, { orgId, pageSize: 100, pageToken: '' }, { pageParamKey: 'pageToken', getNextPageParam: (last) => last.nextPageToken || undefined })
  const members = (list.data?.pages ?? []).flatMap((p) => p.members)

  return (
    <section aria-labelledby="org-members" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <h2 id="org-members" className="font-semibold">
        Members
      </h2>
      <p className="text-muted-foreground text-xs">The people with a role in this org. This list is read-only.</p>
      <QueryError error={list.isError ? list.error : undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading members…" />
      ) : list.isError && !list.data ? null : (
        <>
          <DataTable caption="Members" columns={columns} data={members} getRowId={(m) => m.userId} empty={<EmptyState icon={UsersIcon} title="No members yet." />} />
          {list.hasNextPage && (
            <Button type="button" variant="outline" size="sm" className="justify-self-start" disabled={list.isFetchingNextPage} onClick={() => void list.fetchNextPage()}>
              Show more members
            </Button>
          )}
        </>
      )}
    </section>
  )
}
