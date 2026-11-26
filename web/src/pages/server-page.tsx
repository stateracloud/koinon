// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { useState, type ReactNode } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { OutwardSwitch } from '@/components/outward-switch'
import { RequireAdmin } from '@/components/require-admin'
import { ServerClass } from '@/components/server-class'
import { ServerForm } from '@/components/server-form'
import { HealthBadge, ServerStateBadges, ToolRow, ToolsWaiting } from '@/components/server-tools'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { InlineError, QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Health, McpServerService, type Server, type Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { OutwardTargetKind } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { useMcpActions, type McpActions } from '@/hooks/use-mcp-actions'
import { useFeedback } from '@/hooks/use-feedback'
import { serverDeletable, serverEditable, sourceLabel, summarizeTools, summaryText, tierExplanation, toolsOf, transportLabel } from '@/lib/mcp-servers'
import { useOrg } from '@/lib/org-context'
import { availabilityMinutes, formatTime } from '@/lib/providers'
import { toast } from '@/lib/toast'

const route = getRouteApi('/app/org/mcp-servers/$serverId')

/** ServerPage is one remote MCP server: its settings, its outward switch, and its tools with their tiers and reviews (ADR 0057, ADR 0125). */
export function ServerPage() {
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-4">
      <RequireAdmin what="remote MCP servers">
        <ServerContent />
      </RequireAdmin>
    </div>
  )
}

function ServersBreadcrumb({ name }: { name: string }) {
  return (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink asChild>
            <Link to="/mcp-servers">MCP servers</Link>
          </BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbSeparator />
        <BreadcrumbItem>
          <BreadcrumbPage>{name}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  )
}

function ServerContent() {
  const { serverId } = route.useParams()
  const { org } = useOrg()
  const orgId = org.orgId
  const servers = useQuery(McpServerService.method.listServers, { orgId })
  const tools = useQuery(McpServerService.method.listTools, { orgId, serverId })
  const actions = useMcpActions(orgId)
  const feedback = useFeedback()
  const [editing, setEditing] = useState(false)
  const navigate = useNavigate()
  const server = servers.data?.servers.find((s) => s.id === serverId)
  const failed = servers.error ?? tools.error

  // After "Connect server", the cached list can be older than the new server: wait for the refetch.
  if (servers.isPending || tools.isPending || (!server && servers.isFetching)) return <SkeletonRows label="Loading server…" />
  if (!server) {
    return (
      <>
        <PageHeader title="Server not found" breadcrumb={<ServersBreadcrumb name="Not found" />} />
        <QueryError error={failed ?? undefined} />
        {!failed && <EmptyState title="This server does not exist." description="Another admin may have deleted it." action={<Link to="/mcp-servers" className="underline">Back to MCP servers</Link>} />}
      </>
    )
  }
  const s = server
  const list = toolsOf(tools.data?.tools ?? [], s.id)
  const editable = serverEditable(s)

  return (
    <>
      <PageHeader
        title={s.name}
        breadcrumb={<ServersBreadcrumb name={s.name} />}
        description={
          <div className="flex flex-wrap items-center gap-2">
            <HealthBadge health={s.health} />
            <ServerStateBadges server={s} />
            <span>{sourceLabel(s)}</span>
          </div>
        }
        actions={
          !editing && (
            <>
              {editable && (
                <Button variant="outline" size="sm" aria-label={`Edit server ${s.name}`} onClick={() => setEditing(true)}>
                  Edit
                </Button>
              )}
              {serverDeletable(s) && (
                <ConfirmAction
                  label="Delete"
                  subject={`server ${s.name}`}
                  consequence={`Delete ${s.name} with its ${list.length} ${list.length === 1 ? 'tool' : 'tools'}? Agents that were allowed to use it lose it.`}
                  confirmLabel="Delete server"
                  onConfirm={async () => {
                    await actions.deleteServer(s.id)
                    toast(`Deleted server ${s.name}.`)
                    await navigate({ to: '/mcp-servers' })
                  }}
                  onError={feedback.setError}
                />
              )}
            </>
          )
        }
      />
      <InlineError message={feedback.error} />
      <QueryError error={failed ?? undefined} />
      {editing && (
        <ServerForm
          server={s}
          actions={actions}
          onDone={() => setEditing(false)}
          onSaved={() => toast(`Saved. A new credential reaches the gateway in about ${availabilityMinutes} minutes.`)}
        />
      )}
      <ServerFacts server={s} />
      <ServerNotices server={s} editable={editable} />
      <Section title="Outward calls">
        <p className="text-muted-foreground text-sm">Switch off to stop every call, probe and run offer of this server at once. Its settings and credential stay.</p>
        <OutwardSwitch kind={OutwardTargetKind.MCP_SERVER} targetId={s.id} subject={`server ${s.name}`} disabled={s.adminDisabled} />
      </Section>
      {!s.removed && <ServerClass key={`${s.kind}/${s.dataClass.join()}`} server={s} actions={actions} />}
      {/* Without the tool list, "No tools yet" would be wrong: the error above says why it is missing. */}
      {!tools.error && <ToolsSection server={s} tools={list} actions={actions} onRefused={feedback.setError} />}
    </>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section aria-label={title} className="bg-card grid gap-2 rounded-md border p-4">
      <h2 className="text-base font-semibold">{title}</h2>
      {children}
    </section>
  )
}

function ServerFacts({ server: s }: { server: Server }) {
  return (
    <dl className="bg-card grid grid-cols-[max-content_minmax(0,1fr)] gap-x-4 gap-y-1 rounded-md border p-4 text-sm">
      <dt className="text-muted-foreground">URL</dt>
      <dd className="break-all">{s.url}</dd>
      <dt className="text-muted-foreground">Transport</dt>
      <dd>{transportLabel(s.transport)}</dd>
      <dt className="text-muted-foreground">Credential</dt>
      <dd>{s.credentialLostAt ? 'Lost; enter it again' : s.hasCredential ? 'Stored encrypted; it cannot be shown' : 'None'}</dd>
      <dt className="text-muted-foreground">Certificate</dt>
      <dd>
        {s.insecureSkipVerify ? (
          <span className="text-destructive font-medium">Not verified: the server&apos;s identity is not checked (insecure)</span>
        ) : s.caBundle ? (
          'Only the CA given for this server is trusted'
        ) : (
          'Verified against the public roots'
        )}
      </dd>
      <dt className="text-muted-foreground">Gateway target</dt>
      <dd className="break-all">
        <code>{s.gatewayTarget}</code>
      </dd>
      <dt className="text-muted-foreground">Health checked</dt>
      <dd>{formatTime(s.healthCheckedAt, 'Not yet')}</dd>
      <dt className="text-muted-foreground">Tools synced</dt>
      <dd>{formatTime(s.toolsSyncedAt, 'Not yet')}</dd>
    </dl>
  )
}

function ServerNotices({ server: s, editable }: { server: Server; editable: boolean }) {
  return (
    <>
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
    </>
  )
}

function ToolsSection({ server: s, tools, actions, onRefused }: { server: Server; tools: Tool[]; actions: McpActions; onRefused: (m: string) => void }) {
  return (
    <Section title="Tools">
      <p className="text-muted-foreground text-xs">
        {summaryText(summarizeTools(tools))}. {tierExplanation} A tool that waits for review is hidden from agents until an admin approves its text.
      </p>
      <ToolsWaiting server={s} tools={tools} actions={actions} onNotice={(m) => toast(m)} onRefused={onRefused} />
      {tools.length === 0 ? (
        <p className="text-muted-foreground text-sm">No tools yet. They show here after Koinon first reaches the server.</p>
      ) : (
        <ul className="grid gap-2" aria-label={`Tools of ${s.name}`}>
          {tools.map((t) => (
            <li key={t.name} className="rounded-md border p-3">
              <ToolRow server={s} tool={t} actions={actions} onRefused={onRefused} />
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}
