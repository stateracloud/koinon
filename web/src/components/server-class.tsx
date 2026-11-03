// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import type { Server } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { McpActions } from '@/hooks/use-mcp-actions'
import { dataLegs, kindLabel, legsText, serverKinds, toggleLeg } from '@/lib/mcp-servers'

/**
 * ServerClass shows what a server exposes to the leak guard (ADR 0084, #350) and lets an admin change
 * it. It works on every server, including one declared by a Secret: the Secret does not carry the
 * class, so the class is set here and survives the Secret being applied again.
 */
export function ServerClass({ server: s, actions }: { server: Server; actions: Pick<McpActions, 'setServerClass'> }) {
  const [editing, setEditing] = useState(false)
  const [kind, setKind] = useState(s.kind)
  const [legs, setLegs] = useState<string[]>([...s.dataClass])
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  function open() {
    setKind(s.kind)
    setLegs([...s.dataClass])
    setError(undefined)
    setEditing(true)
  }
  async function save() {
    setPending(true)
    setError(undefined)
    try {
      await actions.setServerClass(s.id, kind, legs)
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
          Data class: {kindLabel(s.kind)}; legs: {legsText(s.dataClass)}.
        </span>
        <Button variant="outline" size="sm" aria-label={`Change data class of ${s.name}`} onClick={open}>
          Change class
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
