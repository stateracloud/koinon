// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useId, useState, type FormEvent } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ConnectionService, type UnlinkedMCPServer } from '@/gen/koinon/connection/v1alpha1/connection_pb'

/**
 * ConnectionMcpHosts lists the extra MCP hosts of a code-host connection (#240): host names besides
 * the connection's own and its connector's, where an MCP server may act through the connection. Every
 * member sees the list; admins and owners add and remove hosts. The server checks the host, so the
 * form only shows its refusal.
 */
export function ConnectionMcpHosts({ orgId, connectionId, hosts, canEdit }: { orgId: string; connectionId: string; hosts: string[]; canEdit: boolean }) {
  const id = useId()
  const queryClient = useQueryClient()
  const [host, setHost] = useState('')
  const [error, setError] = useState<string | null>(null)
  const onSuccess = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
  const add = useMutation(ConnectionService.method.addConnectionMCPHost, { onSuccess })
  const remove = useMutation(ConnectionService.method.removeConnectionMCPHost, { onSuccess })

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    try {
      await add.mutateAsync({ orgId, connectionId, host: host.trim() })
      setHost('')
    } catch (err) {
      setError(`Could not add the host: ${ConnectError.from(err).rawMessage}`)
    }
  }

  return (
    <section aria-labelledby={`${id}-h`} className="bg-card grid gap-3 rounded-lg border p-4">
      <h2 id={`${id}-h`} className="font-semibold">
        MCP server hosts
      </h2>
      <p className="text-muted-foreground text-sm">
        An MCP server can be linked to this connection only when its https URL names this connection&apos;s host, its connector&apos;s MCP host, or one of
        the hosts below. A host matches by exact name, without the port.
      </p>
      {hosts.length === 0 ? (
        <p className="text-muted-foreground text-sm">No extra hosts.</p>
      ) : (
        <ul className="grid gap-1 text-sm" aria-label="Extra MCP hosts">
          {hosts.map((h) => (
            <li key={h} className="flex flex-wrap items-center gap-2">
              <code className="text-xs">{h}</code>
              {canEdit && (
                <RemoveHost
                  host={h}
                  preview={(dryRun) => remove.mutateAsync({ orgId, connectionId, host: h, dryRun })}
                  onError={(message) => setError(`Could not remove the host: ${message}`)}
                />
              )}
            </li>
          ))}
        </ul>
      )}
      {canEdit && (
        <form onSubmit={submit} className="flex flex-wrap items-end gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-host`}>MCP host name</Label>
            <Input id={`${id}-host`} value={host} placeholder="gitea-mcp.example.com" autoComplete="off" onChange={(e) => setHost(e.target.value)} />
          </div>
          <Button type="submit" size="sm" disabled={add.isPending || host.trim() === ''}>
            {add.isPending ? 'Adding…' : 'Add host'}
          </Button>
        </form>
      )}
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
    </section>
  )
}

/**
 * RemoveHost asks before a host is removed, and says which MCP servers the removal unlinks: the server
 * answers a dry run with that list, so the admin sees it before confirming. A host added again does not
 * restore a link.
 */
function RemoveHost({ host, preview, onError }: { host: string; preview: (dryRun: boolean) => Promise<{ unlinkedServers: UnlinkedMCPServer[] }>; onError: (message: string) => void }) {
  const [servers, setServers] = useState<UnlinkedMCPServer[] | null>(null)
  const [failed, setFailed] = useState(false)
  let consequence = `Checking which MCP servers removing ${host} unlinks…`
  if (failed) consequence = `Could not check which MCP servers removing ${host} unlinks. Servers linked through it lose the link and their code-host tool class.`
  else if (servers && servers.length === 0) consequence = `Remove ${host}? No MCP server is linked through it.`
  else if (servers) {
    consequence = `Remove ${host}? These MCP servers lose the link and their code-host tool class, and adding the host again does not restore it: ${servers.map((s) => `${s.name} (${s.url})`).join(', ')}.`
  }
  return (
    <ConfirmAction
      label="Remove"
      subject={`MCP host ${host}`}
      consequence={consequence}
      confirmLabel="Remove host"
      confirmDisabled={servers === null && !failed}
      onOpen={() => {
        setServers(null)
        setFailed(false)
        preview(true).then(
          (res) => setServers(res.unlinkedServers),
          () => setFailed(true),
        )
      }}
      onConfirm={() => preview(false)}
      onError={onError}
    />
  )
}
