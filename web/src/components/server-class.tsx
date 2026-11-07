// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useMemo, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import type { Server } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { McpActions } from '@/hooks/use-mcp-actions'
import { codeHostInfo } from '@/lib/connector'
import { useOrg } from '@/lib/org-context'
import { dataLegs, kindLabel, legsText, serverKinds, toggleLeg } from '@/lib/mcp-servers'

/**
 * ServerClass shows what a server exposes to the leak guard (ADR 0084, old issue 350) and lets an admin change
 * it. It works on every server, including one declared by a Secret: the Secret does not carry the
 * class, so the class is set here and survives the Secret being applied again.
 */
export function ServerClass({ server: s, actions }: { server: Server; actions: Pick<McpActions, 'setServerClass'> }) {
  const { org } = useOrg()
  const connections = useQuery(ConnectionService.method.listConnections, { orgId: org.orgId })
  const types = useQuery(ConnectionService.method.listConnectorTypes, {})
  const linked = connections.data?.connections.find((c) => c.id === s.codeHostConnectionId)
  // Only a code-host connection can be linked (#117); the server's current link stays listed so the
  // select never shows a value it does not offer.
  const linkable = useMemo(() => {
    const hosts = new Set((types.data?.connectorTypes ?? []).filter((t) => codeHostInfo(t)).map((t) => t.id))
    return (connections.data?.connections ?? []).filter((c) => hosts.has(c.connector) || c.id === s.codeHostConnectionId)
  }, [types.data, connections.data, s.codeHostConnectionId])
  const [editing, setEditing] = useState(false)
  const [kind, setKind] = useState(s.kind)
  const [legs, setLegs] = useState<string[]>([...s.dataClass])
  const [connection, setConnection] = useState(s.codeHostConnectionId)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  function open() {
    setKind(s.kind)
    setLegs([...s.dataClass])
    setConnection(s.codeHostConnectionId)
    setError(undefined)
    setEditing(true)
  }
  async function save() {
    setPending(true)
    setError(undefined)
    try {
      await actions.setServerClass(s.id, kind, legs, connection)
      setEditing(false)
    } catch (e) {
      setError(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }

  if (!editing) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span>
          Data class: {kindLabel(s.kind)}; legs: {legsText(s.dataClass)}.{' '}
          {s.codeHostConnectionId ? `Code-host connection: ${linked?.displayName ?? s.codeHostConnectionId}.` : 'No code-host connection: its code-host tools count as external, free text.'}
        </span>
        <Button variant="outline" size="sm" onClick={open}>
          {/* The name starts with the visible text (WCAG 2.5.3, label in name); the server's name is for screen readers. */}
          Change data class{' '}<span className="sr-only">of {s.name}</span>
        </Button>
      </div>
    )
  }
  return (
    <form
      aria-label={`Data class of ${s.name}`}
      className="grid gap-2 rounded-md border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        void save()
      }}
    >
      <div className="grid gap-1.5">
        <Label htmlFor={`class-kind-${s.id}`}>Kind</Label>
        <NativeSelect id={`class-kind-${s.id}`} value={kind} onChange={(e) => setKind(e.target.value)}>
          {serverKinds.map((k) => (
            <option key={k.value} value={k.value}>
              {k.label}
            </option>
          ))}
        </NativeSelect>
      </div>
      {dataLegs.map((l) => (
        <label key={l.value} className="flex items-center gap-2 text-sm" title={l.help}>
          <input type="checkbox" checked={legs.includes(l.value)} onChange={(e) => setLegs(toggleLeg(legs, l.value, e.target.checked))} />
          {l.label}
        </label>
      ))}
      <div className="grid gap-1.5">
        <Label htmlFor={`class-connection-${s.id}`}>Code-host connection</Label>
        <NativeSelect id={`class-connection-${s.id}`} value={connection} onChange={(e) => setConnection(e.target.value)}>
          <option value="">None</option>
          {linkable.map((c) => (
            <option key={c.id} value={c.id}>
              {c.displayName} ({c.connector})
            </option>
          ))}
        </NativeSelect>
        <p className="text-muted-foreground text-xs">
          The connection whose repository list a code-host tool of this server is checked against, so a repository of the same name on another host does not count. Without one, its code-host tools count as external, free text. A changed
          server URL clears it.
        </p>
      </div>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" size="sm" disabled={pending} onClick={() => setEditing(false)}>
          Cancel
        </Button>
        <Button type="submit" size="sm" disabled={pending}>
          Save class
        </Button>
      </div>
    </form>
  )
}
