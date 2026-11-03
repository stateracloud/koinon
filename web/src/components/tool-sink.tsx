// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import type { Server, Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { McpActions } from '@/hooks/use-mcp-actions'
import { buildSink, isDefaultSink, type ResultLeg, type SinkFormState, sinkClasses, sinkDerivations, sinkFormOf, sinkText, validateSink } from '@/lib/mcp-servers'

const resultLegs: { value: ResultLeg; label: string }[] = [
  { value: 'server', label: 'As the server' },
  { value: 'yes', label: 'Yes' },
  { value: 'no', label: 'No' },
]

/**
 * ToolSink shows where a tool's calls go for the leak guard (ADR 0091) and lets an admin declare it:
 * a fixed sink class, or a class decided per call from the address arguments, and what the tool's
 * results carry. A tool nobody declared is external-high; a changed server URL or tool puts it back.
 */
export function ToolSink({ server, tool: t, actions }: { server: Server; tool: Tool; actions: Pick<McpActions, 'setToolSink'> }) {
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState<SinkFormState>(() => sinkFormOf(t.sink))
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const id = `sink-${server.id}-${t.name}`

  function open() {
    setForm(sinkFormOf(t.sink))
    setError(undefined)
    setEditing(true)
  }
  async function save() {
    const bad = validateSink(form)
    if (bad) {
      setError(bad)
      return
    }
    setPending(true)
    setError(undefined)
    try {
      await actions.setToolSink(server.id, t.name, buildSink(form))
      setEditing(false)
    } catch (e) {
      setError(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }
  const set = (patch: Partial<SinkFormState>) => setForm((f) => ({ ...f, ...patch }))
  const derivation = sinkDerivations.find((d) => d.value === form.derivation)

  if (!editing) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span>Where calls go: {sinkText(t.sink)}.</span>
        <Button variant="outline" size="sm" aria-label={`Change sink class of ${t.name}`} onClick={open}>
          {isDefaultSink(t.sink) ? 'Declare' : 'Change'}
        </Button>
      </div>
    )
  }
  return (
    <form
      aria-label={`Sink class of ${t.name}`}
      className="grid gap-2 rounded-md border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        void save()
      }}
    >
      <div className="grid gap-1.5">
        <Label htmlFor={`${id}-derivation`}>How the class is decided</Label>
        <NativeSelect id={`${id}-derivation`} value={form.derivation} onChange={(e) => set({ derivation: e.target.value })}>
          {sinkDerivations.map((d) => (
            <option key={d.value} value={d.value}>
              {d.label}
            </option>
          ))}
        </NativeSelect>
      </div>
      {form.derivation === 'fixed' && (
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-class`}>Sink class</Label>
          <NativeSelect id={`${id}-class`} value={form.sinkClass} onChange={(e) => set({ sinkClass: e.target.value })}>
            {sinkClasses.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </NativeSelect>
          <p className="text-muted-foreground text-xs">{sinkClasses.find((c) => c.value === form.sinkClass)?.help}</p>
        </div>
      )}
      <div className="grid gap-1.5">
        <Label htmlFor={`${id}-args`}>Address arguments</Label>
        <Input id={`${id}-args`} value={form.addressArgs} placeholder="owner, repo" onChange={(e) => set({ addressArgs: e.target.value })} />
        <p className="text-muted-foreground text-xs">{derivation?.hint || 'The arguments that say where a call goes, comma separated. Every other argument is payload.'} A call whose address does not parse is treated as external, free text.</p>
      </div>
      <div className="grid gap-1.5 sm:grid-cols-2">
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-untrusted`}>Results carry untrusted input</Label>
          <NativeSelect id={`${id}-untrusted`} value={form.resultUntrustedInput} onChange={(e) => set({ resultUntrustedInput: e.target.value as ResultLeg })}>
            {resultLegs.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </NativeSelect>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-private`}>Results carry private data</Label>
          <NativeSelect id={`${id}-private`} value={form.resultPrivateData} onChange={(e) => set({ resultPrivateData: e.target.value as ResultLeg })}>
            {resultLegs.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </NativeSelect>
        </div>
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
          Save sink class
        </Button>
      </div>
    </form>
  )
}
