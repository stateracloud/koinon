// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useInfiniteQuery } from '@connectrpc/connect-query'
import { useId, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { useDebounced } from '@/hooks/use-debounced'

/**
 * InstallationRepos lists the repositories a GitHub connection's App
 * installation can access, searchable by owner/name and paged as GitHub
 * pages them: what an agent on this connection can be bound to. Admins and
 * owners.
 */
export function InstallationRepos({ orgId, connectionId, noun = 'the installation can access' }: { orgId: string; connectionId: string; noun?: string }) {
  const id = useId()
  const [search, setSearch] = useState('')
  const query = useDebounced(search.trim())
  const list = useInfiniteQuery(
    ConnectionService.method.listInstallationRepos,
    { orgId, connectionId, query, pageToken: '' },
    { pageParamKey: 'pageToken', getNextPageParam: (last) => last.nextPageToken || undefined, staleTime: 30_000 },
  )
  const pages = list.data?.pages ?? []
  const repos = pages.flatMap((p) => p.repos)
  const total = pages[0]?.totalCount

  return (
    <section aria-labelledby={`${id}-h`} className="bg-card grid gap-3 rounded-lg border p-4">
      <div className="flex flex-wrap items-baseline gap-2">
        <h2 id={`${id}-h`} className="font-semibold">
          Repositories
        </h2>
        {total !== undefined && (
          <span className="text-muted-foreground text-sm">
            {total.toString()} {noun}
          </span>
        )}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor={`${id}-search`}>Search repositories</Label>
        <Input id={`${id}-search`} type="search" value={search} placeholder="owner/name" autoComplete="off" onChange={(e) => setSearch(e.target.value)} />
      </div>
      {list.isError ? (
        <p role="alert" className="text-destructive text-sm">
          Could not list the repositories: {ConnectError.from(list.error).rawMessage}
        </p>
      ) : list.isPending ? (
        <p role="status" className="text-sm">
          Loading repositories…
        </p>
      ) : (
        <>
          {repos.length === 0 && !list.hasNextPage && (
            <p className="text-muted-foreground text-sm">{query ? 'No repository matches.' : `${noun.replace(/^./, (ch) => ch.toUpperCase())} no repositories yet.`}</p>
          )}
          {repos.length > 0 && (
            <ul aria-label="Installation repositories" className="grid gap-1 text-sm">
              {repos.map((r) => (
                <li key={r.fullName} className="flex items-center gap-2">
                  <span className="font-mono">{r.fullName}</span>
                  {r.private && <span className="text-muted-foreground rounded border px-1 text-xs">private</span>}
                  {r.defaultBranch && <span className="text-muted-foreground ml-auto text-xs">{r.defaultBranch}</span>}
                </li>
              ))}
            </ul>
          )}
          {list.hasNextPage && (
            <div>
              <Button type="button" variant="outline" size="sm" disabled={list.isFetchingNextPage} onClick={() => void list.fetchNextPage()}>
                {list.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </Button>
            </div>
          )}
        </>
      )}
    </section>
  )
}
