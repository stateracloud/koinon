// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { RequireAdmin } from '@/components/require-admin'
import { LeakGuardSettings } from '@/components/leak-guard-settings'
import { ServerClass } from '@/components/server-class'
import { ToolSink } from '@/components/tool-sink'
import { ServerForm } from '@/components/server-form'
import { OutwardSwitch } from '@/components/outward-switch'
import { OutwardTargetKind } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Health, McpServerService, Tier, type Server, type Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { useMcpActions, type McpActions } from '@/hooks/use-mcp-actions'
import { closeOnly } from '@/lib/editing'
import { useOrg } from '@/lib/org-context'
import { availabilityMinutes, formatTime } from '@/lib/providers'
import { healthLabel, serverDeletable, serverEditable, sourceLabel, suggestsAuto, summarizeTools, summaryText, tierExplanation, tiers, toolsOf, transportLabel } from '@/lib/mcp-servers'

/** ServersPage is where org admins and owners manage the org's remote MCP servers and their tools' tiers (ADR 0057). */
export function ServersPage() {
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <RequireAdmin what="remote MCP servers">
        <ServersContent />
      </RequireAdmin>
    </div>
  )
}

type Editing = { kind: 'new' } | { kind: 'server'; id: string }

function ServersContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  const servers = useQuery(McpServerService.method.listServers, { orgId })
  const tools = useQuery(McpServerService.method.listTools, { orgId, serverId: '' })
  const actions = useMcpActions(orgId)
  const [editing, setEditing] = useState<Editing>()
  const [notice, setNotice] = useState<string>()
  const [refused, setRefused] = useState<string>()
  const saved = () => {
    setRefused(undefined)
    setNotice(`Saved. A new credential reaches the gateway within about ${availabilityMinutes} minutes.`)
  }
  const failed = servers.error ?? tools.error
  const list = servers.data?.servers ?? []
  const allTools = tools.data?.tools ?? []

  return (
    <>
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">MCP servers</h1>
        <Button size="sm" className="ml-auto" onClick={() => setEditing({ kind: 'new' })} disabled={editing?.kind === 'new'}>
          Add server
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        A server is a remote MCP server your agents can call through the gateway, such as a docs or issue-tracker server. Koinon keeps its credential encrypted and hands it only to the gateway; sandboxes never see it. Servers can also be declared with a Kubernetes Secret, which are read-only here (their tools&apos; tiers can still be set). The tools of a new server start in the Ask tier. A new or changed credential takes about {availabilityMinutes} minutes to reach the gateway.
      </p>
      <p className="text-muted-foreground text-sm">{tierExplanation} A server that stops answering is Unhealthy and its tools are withdrawn from agents until it recovers.</p>
      {editing?.kind === 'new' && <ServerForm actions={actions} onDone={closeOnly(setEditing, { kind: 'new' })} onSaved={saved} />}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
      {failed && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(failed).rawMessage}</AlertDescription>
        </Alert>
      )}
      {servers.isPending || tools.isPending ? (
        <p role="status">Loading servers…</p>
      ) : list.length === 0 ? (
        <p className="text-muted-foreground text-sm">No servers yet. Add a remote MCP server so agents can use its tools.</p>
      ) : (
        <ul className="grid gap-4" aria-label="Servers">
          {list.map((s) => (
            <li key={s.id}>
              <ServerCard
                server={s}
                tools={toolsOf(allTools, s.id)}
                editing={editing?.kind === 'server' && editing.id === s.id}
                edit={() => setEditing({ kind: 'server', id: s.id })}
                done={closeOnly(setEditing, { kind: 'server', id: s.id })}
                actions={actions}
                onSaved={saved}
                onNotice={(m) => {
                  setRefused(undefined)
                  setNotice(m)
                }}
                onRefused={(m) => {
                  setNotice(undefined)
                  setRefused(m)
                }}
              />
            </li>
          ))}
        </ul>
      )}
      <LeakGuardSettings orgId={orgId} />
    </>
  )
}

function HealthBadge({ health }: { health: Health }) {
  const label = healthLabel(health)
  return <Badge variant={health === Health.UNHEALTHY ? 'destructive' : health === Health.HEALTHY ? 'secondary' : 'outline'}>{label}</Badge>
}

function ServerCard({
  server: s,
  tools,
  editing,
  edit,
  done,
  actions,
  onSaved,
  onNotice,
  onRefused,
}: {
  server: Server
  tools: Tool[]
  editing: boolean
  edit: () => void
  done: () => void
  actions: McpActions
  onSaved: () => void
  onNotice: (m: string) => void
  onRefused: (m: string) => void
}) {
  const editable = serverEditable(s)
  const summary = summarizeTools(tools)
  return (
    <section aria-labelledby={`server-${s.id}`} className="bg-card grid gap-3 rounded-md border p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h2 id={`server-${s.id}`} className="text-base font-semibold">
          {s.name}
        </h2>
        <HealthBadge health={s.health} />
        {s.removed && <Badge variant="destructive">Removed</Badge>}
        {!s.removed && s.disabledReason && <Badge variant="destructive">Disabled</Badge>}
        {!s.removed && s.credentialLostAt && <Badge variant="destructive">Credential lost</Badge>}
        <span className="text-muted-foreground ml-auto text-sm">{sourceLabel(s)}</span>
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">URL</dt>
        <dd className="break-all">{s.url}</dd>
        <dt className="text-muted-foreground">Transport</dt>
        <dd>{transportLabel(s.transport)}</dd>
        <dt className="text-muted-foreground">Credential</dt>
        <dd>{s.credentialLostAt ? 'Lost; enter it again' : s.hasCredential ? 'Stored encrypted; it cannot be shown' : 'None'}</dd>
        <dt className="text-muted-foreground">Gateway target</dt>
        <dd className="break-all">
          <code>{s.gatewayTarget}</code>
        </dd>
        <dt className="text-muted-foreground">Health checked</dt>
        <dd>{formatTime(s.healthCheckedAt, 'Not yet')}</dd>
      </dl>
      {s.health === Health.UNHEALTHY && (
        <Alert variant="destructive" role="note">
          <AlertDescription>
            Unhealthy{s.healthReason ? `: ${s.healthReason}` : ''}. Its tools are withdrawn from agents until it recovers.
          </AlertDescription>
        </Alert>
      )}
      {(s.removed || s.disabledReason) && (
        <Alert variant="destructive" role="note">
          <AlertDescription>
            {s.removed ? 'Removed' : 'Disabled'}: {s.disabledReason || 'its Kubernetes Secret was deleted'}.
          </AlertDescription>
        </Alert>
      )}
      {s.credentialLostAt && !s.removed && (
        <Alert variant="destructive" role="note">
          <AlertDescription>
            {editable
              ? 'The stored credential was lost with a key-encryption key, so agents cannot use this server. Edit it and enter the credential again.'
              : `The stored credential was lost with a key-encryption key, so agents cannot use this server. Koinon reads it again from the Secret ${s.managedBySecret} on its next sync.`}
          </AlertDescription>
        </Alert>
      )}
      {!editable && !s.removed && (
        <p className="text-muted-foreground text-xs">
          This server is declared by the Secret {s.managedBySecret}. Change or delete that Secret to change it here. You can still set its tools&apos; tiers.
        </p>
      )}
      <OutwardSwitch kind={OutwardTargetKind.MCP_SERVER} targetId={s.id} subject={`server ${s.name}`} disabled={s.adminDisabled} />
      {!s.removed && <ServerClass key={`${s.kind}/${s.dataClass.join()}`} server={s} actions={actions} />}
      {editing ? (
        <ServerForm server={s} actions={actions} onDone={done} onSaved={onSaved} />
      ) : (
        (editable || serverDeletable(s)) && (
          <div className="flex flex-wrap gap-2">
            {editable && (
              <Button variant="outline" size="sm" aria-label={`Edit server ${s.name}`} onClick={edit}>
                Edit
              </Button>
            )}
            {serverDeletable(s) && (
              <ConfirmAction
                label="Delete"
                subject={`server ${s.name}`}
                consequence={`Delete ${s.name} with its ${tools.length} ${tools.length === 1 ? 'tool' : 'tools'}? Agents that were allowed to use it lose it.`}
                confirmLabel="Delete server"
                onConfirm={async () => {
                  await actions.deleteServer(s.id)
                  onNotice(`Deleted server ${s.name}.`)
                }}
                onError={onRefused}
              />
            )}
          </div>
        )
      )}

      <div className="grid gap-2 border-t pt-3">
        <h3 className="text-sm font-semibold">Tools</h3>
        <p className="text-muted-foreground text-xs">{summaryText(summary)}</p>
        {tools.length === 0 ? (
          <p className="text-muted-foreground text-sm">No tools yet. They appear after Koinon first reaches the server.</p>
        ) : (
          <ul className="grid gap-2" aria-label={`Tools of ${s.name}`}>
            {tools.map((t) => (
              <li key={t.name} className="rounded-md border p-3">
                <ToolRow server={s} tool={t} actions={actions} onRefused={onRefused} />
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  )
}

function ToolRow({ server, tool: t, actions, onRefused }: { server: Server; tool: Tool; actions: McpActions; onRefused: (m: string) => void }) {
  const [pending, setPending] = useState(false)
  async function run(p: Promise<unknown>) {
    setPending(true)
    try {
      await p
    } catch (e) {
      onRefused(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }
  const idBase = `tool-${server.id}-${t.name}`
  const askTier = t.tier === Tier.ASK || t.tier === Tier.UNSPECIFIED
  return (
    <div className="grid gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <code className="text-sm font-medium break-all">{t.name}</code>
        {t.reviewReason && <Badge variant="outline">Review</Badge>}
        {!t.present && <span className="text-muted-foreground text-xs">no longer listed</span>}
        {t.unavailable && <Badge variant="outline">Not offered</Badge>}
      </div>
      {t.unavailable && (
        <p role="note" className="text-sm text-amber-700 dark:text-amber-400">
          {t.unavailable}. Agents are not offered this tool, whatever its tier.
        </p>
      )}
      {t.description && <p className="text-muted-foreground text-sm">{t.description}</p>}
      {(t.descriptionCapped || t.descriptionCleaned) && <SanitisedNote tool={t} />}
      <fieldset className="flex flex-wrap items-center gap-x-4 gap-y-1" disabled={pending}>
        <legend className="sr-only">Decision for {t.name}</legend>
        {tiers.map((x) => (
          <label key={x.value} className="flex items-center gap-1.5 text-sm" title={x.help}>
            <input
              type="radio"
              name={`${idBase}-tier`}
              value={x.value}
              checked={t.tier === x.value || (t.tier === Tier.UNSPECIFIED && x.value === Tier.ASK)}
              onChange={() => void run(actions.setToolTier(server.id, t.name, x.value))}
            />
            {x.label}
          </label>
        ))}
      </fieldset>
      <div className="grid gap-0.5">
        <label className="flex items-center gap-1.5 text-sm">
          <input
            type="checkbox"
            checked={t.requiresTwoApprovers}
            disabled={pending || !askTier}
            aria-describedby={`${idBase}-two-help`}
            onChange={(e) => void run(actions.setToolTwoApprovers(server.id, t.name, e.target.checked))}
          />
          Require two approvers
        </label>
        <p id={`${idBase}-two-help`} className="text-muted-foreground text-xs">
          Two distinct admins or owners must approve, neither the person who created the task.
          {!askTier && ' This only applies in the Ask tier.'}
        </p>
      </div>
      {!server.removed && <ToolSink key={`${t.sink?.sinkClass}/${t.sink?.derivation}/${t.sink?.addressArgs.join()}`} server={server} tool={t} actions={actions} />}
      {suggestsAuto(t) && (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span>Suggested read-only: the server marks this tool as read-only. That is its own claim, so the tier stays as it is until you choose.</span>
          <Button variant="outline" size="sm" disabled={pending} aria-label={`Set to Allow: ${t.name}`} onClick={() => void run(actions.setToolTier(server.id, t.name, Tier.AUTO))}>
            Set to Allow
          </Button>
        </div>
      )}
      {t.reviewReason && (
        <div className="flex flex-wrap items-center gap-2 text-sm" role="note">
          <span>Review: {t.reviewReason}.</span>
          <Button variant="outline" size="sm" disabled={pending} aria-label={`Mark reviewed: ${t.name}`} onClick={() => void run(actions.acknowledgeReview(server.id, t.name))}>
            Mark reviewed
          </Button>
          <p className="text-muted-foreground w-full text-xs">Reviewing keeps the tool in Ask; set it back to Allow yourself if you trust the new version.</p>
        </div>
      )}
    </div>
  )
}

/** SanitisedNote says Koinon changed what the server published before showing it to agents (ADR 0057, #52). */
function SanitisedNote({ tool }: { tool: Tool }) {
  const parts: string[] = []
  if (tool.descriptionCleaned) parts.push('hidden characters removed or lookalike tags escaped')
  if (tool.descriptionCapped) {
    parts.push(
      tool.descriptionOriginalLength > 0n
        ? `cut to its length cap (the server published ${tool.descriptionOriginalLength} characters)`
        : 'cut to its length cap',
    )
  }
  return (
    <p role="note" className="text-muted-foreground text-xs">
      Koinon sanitised this tool's text: {parts.join('; ')}. Agents see the text shown here.
    </p>
  )
}
