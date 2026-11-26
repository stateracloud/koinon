// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { ToolSink } from '@/components/tool-sink'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Health, Tier, type Server, type Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { McpActions } from '@/hooks/use-mcp-actions'
import { describeError } from '@/lib/api-error'
import { absentUnreviewed, healthLabel, suggestsAuto, tiers, waitsForReview } from '@/lib/mcp-servers'

// The parts of the MCP server pages that show a server's state and decide its tools (ADR 0057, ADR 0125).

export function HealthBadge({ health }: { health: Health }) {
  return <Badge variant={health === Health.UNHEALTHY ? 'destructive' : health === Health.HEALTHY ? 'secondary' : 'outline'}>{healthLabel(health)}</Badge>
}

/** ServerStateBadges are the badges of a server that is not served for a reason other than its health. */
export function ServerStateBadges({ server: s }: { server: Server }) {
  return (
    <>
      {s.removed && <Badge variant="destructive">Removed</Badge>}
      {!s.removed && s.disabledReason && <Badge variant="destructive">Disabled</Badge>}
      {!s.removed && s.credentialLostAt && <Badge variant="destructive">Credential lost</Badge>}
    </>
  )
}

/**
 * ToolsWaiting is the notice of the tools that wait for an admin, with the action that reviews them all
 * (#1216, ADR 0125). The action approves the text shown here, at the hashes shown: a tool whose text
 * changed since makes the server refuse the whole action, and the list refreshes. It also deletes the
 * rows of tools that the server no longer lists and that no admin reviewed.
 */
export function ToolsWaiting({
  server: s,
  tools,
  actions,
  onNotice,
  onRefused,
}: {
  server: Server
  tools: Tool[]
  actions: McpActions
  onNotice: (m: string) => void
  onRefused: (m: string) => void
}) {
  const waiting = tools.filter(waitsForReview)
  const absent = tools.filter(absentUnreviewed)
  if (waiting.length === 0 && absent.length === 0 && !s.unreviewedToolsCappedAt) return null
  const parts: string[] = []
  if (waiting.length) parts.push(`approves the current text of ${waiting.length} ${waiting.length === 1 ? 'tool' : 'tools'} (${waiting.map((t) => t.name).join(', ')}) without changing a tier`)
  if (absent.length) parts.push(`deletes ${absent.length} ${absent.length === 1 ? 'tool' : 'tools'} that the server no longer lists and that no admin reviewed`)
  return (
    <Alert role="note" aria-label={`Tools waiting for review: ${s.name}`}>
      <AlertDescription className="grid gap-2">
        {waiting.length > 0 && (
          <p>
            {waiting.length} {waiting.length === 1 ? 'tool waits' : 'tools wait'} for review. Agents do not get the text of a tool that waits for review until an admin approves it.
          </p>
        )}
        {s.unreviewedToolsCappedAt && <p>This server is at the limit of tools that no admin reviewed. Koinon stores no new tool of it until you review its tools.</p>}
        {parts.length > 0 && (
          <div>
            <ConfirmAction
              label="Review all"
              subject={`tools of ${s.name}`}
              consequence={`This ${parts.join(', and ')}.`}
              confirmLabel="Review all"
              onConfirm={async () => {
                const res = await actions.reviewAllTools(
                  s.id,
                  waiting.map((t) => ({ tool: t.name, toolHash: t.toolHash })),
                )
                onNotice(`Reviewed ${res.tools.length} ${res.tools.length === 1 ? 'tool' : 'tools'} of ${s.name}. Deleted ${res.removed}.`)
              }}
              onError={onRefused}
            />
          </div>
        )}
      </AlertDescription>
    </Alert>
  )
}

export function ToolRow({ server, tool: t, actions, onRefused }: { server: Server; tool: Tool; actions: McpActions; onRefused: (m: string) => void }) {
  const [pending, setPending] = useState(false)
  async function run(p: Promise<unknown>) {
    setPending(true)
    try {
      await p
    } catch (e) {
      onRefused(describeError(e).message)
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
        {waitsForReview(t) && <Badge variant="outline">Waits for review</Badge>}
        {!t.present && <span className="text-muted-foreground text-xs">no longer listed</span>}
        {t.unavailable && <Badge variant="outline">Not offered</Badge>}
      </div>
      {t.unavailable && (
        <p role="note" className="text-sm text-warning">
          {t.unavailable}. Agents are not offered this tool, whatever its tier.
        </p>
      )}
      {t.description && <p className="text-muted-foreground text-sm">{t.description}</p>}
      {(t.descriptionCapped || t.descriptionCleaned) && <SanitisedNote tool={t} />}
      {pendingApproval(t) && <PendingText tool={t} />}
      <fieldset className="flex flex-wrap items-center gap-x-4 gap-y-1" disabled={pending}>
        <legend className="sr-only">Decision for {t.name}</legend>
        {tiers.map((x) => (
          <label key={x.value} className="flex items-center gap-1.5 text-sm" title={x.help}>
            <input
              type="radio"
              name={`${idBase}-tier`}
              value={x.value}
              checked={t.tier === x.value || (t.tier === Tier.UNSPECIFIED && x.value === Tier.ASK)}
              onChange={() => void run(actions.setToolTier(server.id, t.name, x.value, t.toolHash))}
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
          <Button variant="outline" size="sm" disabled={pending} aria-label={`Set to Allow: ${t.name}`} onClick={() => void run(actions.setToolTier(server.id, t.name, Tier.AUTO, t.toolHash))}>
            Set to Allow
          </Button>
        </div>
      )}
      {waitsForReview(t) && (
        <div className="flex flex-wrap items-center gap-2 text-sm" role="note">
          <span>{t.reviewReason ? `Review: ${t.reviewReason}.` : 'New tool: agents do not get it until an admin reviews its text.'}</span>
          <Button variant="outline" size="sm" disabled={pending} aria-label={`Mark reviewed: ${t.name}`} onClick={() => void run(actions.acknowledgeReview(server.id, t.name, t.toolHash))}>
            Mark reviewed
          </Button>
          <p className="text-muted-foreground w-full text-xs">
            {t.reviewReason ? 'Reviewing keeps the tool in Ask; set it back to Allow yourself if you trust the new version.' : 'Reviewing approves the text shown and keeps the tier.'}
          </p>
        </div>
      )}
    </div>
  )
}

/** pendingApproval reports a tool whose observed text nobody approved yet (#1132, #1173). */
function pendingApproval(t: Tool): boolean {
  return t.toolHash !== '' && t.toolHash !== t.approvedHash
}

type PinnedText = { name?: string; title?: string; description?: string; annotations?: unknown; inputSchema?: unknown; outputSchema?: unknown }

function parsePinned(raw: string): PinnedText | undefined {
  try {
    const v: unknown = JSON.parse(raw)
    return v && typeof v === 'object' ? (v as PinnedText) : undefined
  } catch {
    return undefined
  }
}

/**
 * PendingText shows the exact text a decision approves: every field the hash covers (name, title,
 * description, annotations, input and output schema) as the sync observed it, and the hash (#1173). A decision sends that hash, and the server refuses it when the
 * text changed in the meantime, so an admin never approves text they did not see.
 */
function PendingText({ tool }: { tool: Tool }) {
  const pinned = parsePinned(tool.toolJson)
  return (
    <div className="grid gap-1 rounded-md border p-2 text-sm" role="group" aria-label={`Text waiting for approval: ${tool.name}`}>
      <p className="font-medium">Text waiting for approval</p>
      <p className="text-muted-foreground text-xs">A decision on this tool approves this text. If the server changes it before you decide, Koinon refuses the decision and shows the new text.</p>
      {pinned ? (
        <dl className="grid gap-1">
          <dt className="text-muted-foreground text-xs">Name</dt>
          <dd>
            <code className="break-all">{pinned.name ?? tool.name}</code>
          </dd>
          {pinned.title && (
            <>
              <dt className="text-muted-foreground text-xs">Title</dt>
              <dd className="break-words">{pinned.title}</dd>
            </>
          )}
          <dt className="text-muted-foreground text-xs">Description</dt>
          <dd className="break-words whitespace-pre-wrap">{pinned.description || '(none)'}</dd>
          <PinnedJSON label="Annotations" value={pinned.annotations} />
          <PinnedJSON label="Input schema" value={pinned.inputSchema} />
          <PinnedJSON label="Output schema" value={pinned.outputSchema} />
        </dl>
      ) : (
        <p className="text-muted-foreground text-xs">The text could not be read.</p>
      )}
      <p className="text-xs">
        Hash: <code className="break-all">{tool.toolHash}</code>
      </p>
    </div>
  )
}

function PinnedJSON({ label, value }: { label: string; value: unknown }) {
  return (
    <>
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd>
        <pre className="bg-muted max-h-64 overflow-auto rounded p-2 text-xs">{value === undefined ? '(none)' : JSON.stringify(value, null, 2)}</pre>
      </dd>
    </>
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
