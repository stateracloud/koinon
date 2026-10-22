// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useId, useState, type KeyboardEvent } from 'react'
import { Input } from '@/components/ui/input'
import { ConnectionService, type InstallationRepo } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { useDebounced } from '@/hooks/use-debounced'
import { useOrg } from '@/lib/org-context'
import { cn } from '@/lib/utils'

export type RepoPickerProps = {
  /** The GitHub connection whose installation is searched; empty until one is chosen. */
  connectionId: string
  value: string
  onChange: (value: string) => void
  /**
   * Whether the viewer may list the installation's repositories (admins and
   * owners). Without it, or when listing fails, the picker is a plain input.
   */
  searchable: boolean
  'aria-label': string
  'aria-invalid'?: boolean
}

/**
 * RepoPicker is a combobox for one repository as owner/name: typing searches
 * the connection's GitHub App installation (ListInstallationRepos, matched
 * case-insensitively on owner/name) and picking a suggestion fills in
 * GitHub's spelling. Any name can still be typed; the API checks it when the
 * draft is created.
 */
export function RepoPicker({ connectionId, value, onChange, searchable, 'aria-label': label, 'aria-invalid': invalid }: RepoPickerProps) {
  const id = useId()
  const { org } = useOrg()
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(-1)
  const query = useDebounced(value.trim())
  const enabled = searchable && !!connectionId && open
  const list = useQuery(ConnectionService.method.listInstallationRepos, { orgId: org.orgId, connectionId, query }, { enabled, staleTime: 30_000 })
  // Suggestions for an older input are not shown: picking one with the keyboard
  // would take a name the user has typed past.
  const settled = query === value.trim() && !list.isPlaceholderData
  const repos: readonly InstallationRepo[] = enabled && settled ? (list.data?.repos ?? []) : []
  const listId = `${id}-list`
  const shown = enabled
  const searching = !settled || list.isPending

  function pick(r: InstallationRepo) {
    onChange(r.fullName)
    setOpen(false)
    setActive(-1)
  }

  function onKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setOpen(true)
      setActive((a) => Math.min(a + 1, repos.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((a) => Math.max(a - 1, -1))
    } else if (e.key === 'Enter' && open && active >= 0 && repos[active]) {
      e.preventDefault()
      pick(repos[active])
    } else if (e.key === 'Escape' && open) {
      e.preventDefault()
      setOpen(false)
      setActive(-1)
    }
  }

  return (
    <div className="relative w-full">
      <Input
        role="combobox"
        aria-label={label}
        aria-invalid={invalid}
        aria-autocomplete="list"
        aria-expanded={shown}
        aria-controls={listId}
        aria-activedescendant={shown && active >= 0 ? `${id}-opt-${active}` : undefined}
        value={value}
        placeholder="owner/name"
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => {
          onChange(e.target.value)
          setOpen(true)
          setActive(-1)
        }}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={onKeyDown}
      />
      {shown && (
        <div className="bg-popover text-popover-foreground absolute z-10 mt-1 w-full rounded-md border shadow-md">
          <ul id={listId} role="listbox" aria-label={`Repositories for ${label}`} className="max-h-64 overflow-y-auto py-1 text-sm">
            {repos.map((r, i) => (
              <li
                key={r.fullName}
                id={`${id}-opt-${i}`}
                role="option"
                aria-selected={i === active}
                className={cn('flex cursor-pointer items-center gap-2 px-3 py-1.5', i === active && 'bg-accent text-accent-foreground')}
                // Keep the focus in the input, so blur does not close the list before the click.
                onMouseDown={(e) => e.preventDefault()}
                onMouseEnter={() => setActive(i)}
                onClick={() => pick(r)}
              >
                <span className="font-mono">{r.fullName}</span>
                {r.private && <span className="text-muted-foreground rounded border px-1 text-xs">private</span>}
                {r.defaultBranch && <span className="text-muted-foreground ml-auto text-xs">{r.defaultBranch}</span>}
              </li>
            ))}
          </ul>
          <p className="text-muted-foreground border-t px-3 py-1.5 text-xs" role="status">
            {list.isError && settled
              ? `Could not list the installation’s repositories (${ConnectError.from(list.error).rawMessage}); type owner/name.`
              : searching
                ? 'Searching the installation…'
                : repos.length === 0
                  ? 'No repository of the installation matches; you can still type owner/name.'
                  : list.data?.nextPageToken
                    ? 'More repositories match; keep typing to narrow the list.'
                    : query
                      ? `${repos.length} of the installation’s ${list.data?.totalCount ?? repos.length} repositories match.`
                      : `The installation’s ${repos.length} ${repos.length === 1 ? 'repository' : 'repositories'}.`}
          </p>
        </div>
      )}
    </div>
  )
}
