// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useId, useState, type FormEvent } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'

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
  const [error, setError] = useState<string>()
  const onSuccess = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
  const add = useMutation(ConnectionService.method.addConnectionMCPHost, { onSuccess })
  const remove = useMutation(ConnectionService.method.removeConnectionMCPHost, { onSuccess })

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError(undefined)
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
                <ConfirmAction
                  label="Remove"
                  subject={`MCP host ${h}`}
                  consequence={`Remove ${h}? MCP servers at ${h} linked to this connection lose the link and their code-host tool class.`}
                  confirmLabel="Remove host"
                  onConfirm={() => remove.mutateAsync({ orgId, connectionId, host: h })}
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
