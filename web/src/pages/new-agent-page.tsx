// SPDX-License-Identifier: Apache-2.0

import { useMutation, useQuery } from '@connectrpc/connect-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { useMemo, type ReactNode } from 'react'
import { AgentWizard } from '@/components/agent-wizard'
import { BlankAgentForm } from '@/components/blank-agent-form'
import { TemplateNotSaved } from '@/components/save-as-template'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { PageHeader } from '@/components/ui/page-header'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { ProviderService } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { TemplateScope, TemplateService, type Template } from '@/gen/koinon/template/v1alpha1/template_pb'
import { useFinishDraft } from '@/hooks/use-new-agent'
import { describeError } from '@/lib/api-error'
import { isApprover } from '@/lib/approvals'
import { codeHostInfo, setupGuide } from '@/lib/connector'
import { useOrg } from '@/lib/org-context'

const templateRoute = getRouteApi('/app/org/agents/new/template/$templateId')

/** Shell is the frame of the three new-agent pages: the way back, the title and the viewer check. */
function Shell({ back, title, description, children }: { back: { to: '/' | '/agents/new'; label: string }; title: string; description?: ReactNode; children: ReactNode }) {
  const { org } = useOrg()
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to={back.to} className="text-muted-foreground text-sm hover:underline">
        ← {back.label}
      </Link>
      <PageHeader title={title} description={description} />
      {org.role < OrgRole.MEMBER ? (
        <Alert role="alert">
          <AlertDescription>Viewers cannot create agents.</AlertDescription>
        </Alert>
      ) : (
        children
      )}
    </div>
  )
}

function LoadError({ error }: { error: unknown }) {
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription>{describeError(error).message}</AlertDescription>
    </Alert>
  )
}

/**
 * NewAgentPage is the gallery of "New agent" (ADR 0126 section 5): "Blank agent", then the
 * platform templates and the org's own templates, in groups. Every path makes a draft (ADR 0020).
 */
export function NewAgentPage() {
  const { org } = useOrg()
  const templates = useQuery(TemplateService.method.listTemplates, { orgId: org.orgId }, { enabled: org.role >= OrgRole.MEMBER })
  const list = templates.data?.templates ?? []
  const platform = list.filter((t) => t.scope !== TemplateScope.ORG)
  const own = list.filter((t) => t.scope === TemplateScope.ORG)
  return (
    <Shell back={{ to: '/', label: 'Agents' }} title="New agent" description="Start from a template, or from a blank form. Every new agent is a draft until an org admin or owner activates it.">
      {templates.error && <LoadError error={templates.error} />}
      <Group title="Start from nothing">
        <GalleryCard to={{ to: '/agents/new/blank' }} title="Blank agent" description="One form for the name, instructions, model, access, triggers and limits." />
      </Group>
      {templates.isPending ? (
        <p role="status">Loading templates…</p>
      ) : (
        <>
          <Group title="Platform templates">
            {platform.length === 0 ? <p className="text-muted-foreground text-sm">This install ships no templates.</p> : platform.map((t) => <TemplateCard key={t.id} template={t} />)}
          </Group>
          <Group title="Org templates">
            {own.length === 0 ? (
              <p className="text-muted-foreground text-sm">None yet. Use “Save as template” when you create an agent, or on a draft.</p>
            ) : (
              own.map((t) => <TemplateCard key={t.id} template={t} />)
            )}
          </Group>
        </>
      )}
    </Shell>
  )
}

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section aria-label={title} className="grid gap-2">
      <h2 className="font-semibold">{title}</h2>
      <div className="grid gap-3 sm:grid-cols-2">{children}</div>
    </section>
  )
}

type CardTarget = { to: '/agents/new/blank' } | { to: '/agents/new/template/$templateId'; params: { templateId: string } }

function GalleryCard({ to, title, meta, description }: { to: CardTarget; title: string; meta?: string; description: string }) {
  return (
    <Link {...to} className="bg-card hover:border-primary focus-visible:ring-ring grid content-start gap-1 rounded-lg border p-4 text-sm focus-visible:ring-2 focus-visible:outline-none">
      <span className="font-medium">
        {title} {meta && <span className="text-muted-foreground text-xs font-normal">{meta}</span>}
      </span>
      <span className="text-muted-foreground">{description}</span>
    </Link>
  )
}

function TemplateCard({ template: t }: { template: Template }) {
  return <GalleryCard to={{ to: '/agents/new/template/$templateId', params: { templateId: t.id } }} title={t.displayName} meta={t.version ? `v${t.version}` : undefined} description={t.description || 'No description.'} />
}

/** useConnectorInfo loads the connections and what each connector type says about its targets and credential. */
function useConnectorInfo(orgId: string) {
  const connections = useQuery(ConnectionService.method.listConnections, { orgId })
  const types = useQuery(ConnectionService.method.listConnectorTypes, {})
  const patterns = useMemo(
    () => new Map((types.data?.connectorTypes ?? []).flatMap((t) => (setupGuide(t).targetPattern ? [[t.id, setupGuide(t).targetPattern!] as const] : []))),
    [types.data],
  )
  const codeHosts = useMemo(
    () =>
      new Map(
        (types.data?.connectorTypes ?? []).flatMap((t) => {
          const h = codeHostInfo(t)
          return h ? [[t.id, h] as const] : []
        }),
      ),
    [types.data],
  )
  return { connections, types, patterns, codeHosts }
}

/** TemplateAgentPage is the wizard of one template: Harness, Access, Review. */
export function TemplateAgentPage() {
  const { templateId } = templateRoute.useParams()
  const { org } = useOrg()
  const orgId = org.orgId
  const templates = useQuery(TemplateService.method.listTemplates, { orgId })
  const { connections, patterns, codeHosts } = useConnectorInfo(orgId)
  const { finish, saving, failed } = useFinishDraft(orgId)
  const create = useMutation(AgentService.method.createAgentFromTemplate)
  const template = templates.data?.templates.find((t) => t.id === templateId)
  const loadError = templates.error ?? connections.error

  return (
    <Shell back={{ to: '/agents/new', label: 'New agent' }} title={template ? `New agent from ${template.displayName}` : 'New agent from a template'}>
      {loadError && <LoadError error={loadError} />}
      {templates.isPending || connections.isPending ? (
        <p role="status">Loading the template…</p>
      ) : !template ? (
        !loadError && (
          <Alert variant="destructive" role="alert">
            <AlertDescription>
              This template does not exist, or it changed.{' '}
              <Link to="/agents/new" className="underline">
                Choose a template again
              </Link>
              .
            </AlertDescription>
          </Alert>
        )
      ) : (
        connections.data && (
          <>
            {failed && <TemplateNotSaved {...failed} />}
            <AgentWizard
              template={template}
              connections={connections.data.connections}
              targetPatterns={patterns}
              codeHosts={codeHosts}
              canConnect={isApprover(org.role)}
              canListRepos={isApprover(org.role)}
              pending={create.isPending || saving || !!failed}
              error={create.error ? describeError(create.error).message : undefined}
              onCreate={(a, choice) => create.mutateAsync({ orgId, ...a }).then((res) => finish(res.agent, choice), () => undefined)}
            />
          </>
        )
      )}
    </Shell>
  )
}

/** BlankAgentPage is "Blank agent": one form in sections, no template (CreateAgent). */
export function BlankAgentPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const admin = isApprover(org.role)
  const { connections, types } = useConnectorInfo(orgId)
  // The served models are for admins and owners (ProviderService); a member keeps the org default.
  const models = useQuery(ProviderService.method.listModels, { orgId }, { enabled: admin })
  const { finish, saving, failed } = useFinishDraft(orgId)
  const create = useMutation(AgentService.method.createAgent)
  const loadError = connections.error ?? types.error

  return (
    <Shell back={{ to: '/agents/new', label: 'New agent' }} title="Blank agent" description="Every field of the agent on one page. The draft is checked as a template is.">
      {loadError && <LoadError error={loadError} />}
      {connections.isPending || types.isPending ? (
        <p role="status">Loading…</p>
      ) : (
        connections.data &&
        types.data && (
          <>
            {failed && <TemplateNotSaved {...failed} />}
            <BlankAgentForm
              orgId={orgId}
              connectorTypes={types.data.connectorTypes}
              connections={connections.data.connections}
              models={admin && models.data ? [...new Set(models.data.models.filter((m) => !m.disabledReason).map((m) => m.name))] : undefined}
              canListRepos={admin}
              pending={create.isPending || saving || !!failed}
              error={create.error ? describeError(create.error).message : undefined}
              onCreate={(definition, choice) => create.mutateAsync({ orgId, definition }).then((res) => finish(res.agent, choice), () => undefined)}
            />
          </>
        )
      )}
    </Shell>
  )
}
