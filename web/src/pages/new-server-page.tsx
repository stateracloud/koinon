// SPDX-License-Identifier: Apache-2.0

import { Link, useNavigate } from '@tanstack/react-router'
import { useRef } from 'react'
import { RequireAdmin } from '@/components/require-admin'
import { ServerForm } from '@/components/server-form'
import { Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from '@/components/ui/breadcrumb'
import { PageHeader } from '@/components/ui/page-header'
import { useMcpActions } from '@/hooks/use-mcp-actions'
import { useOrg } from '@/lib/org-context'
import { availabilityMinutes } from '@/lib/providers'
import { toast } from '@/lib/toast'

/** NewServerPage connects a remote MCP server to the org (ADR 0057, ADR 0126 section 6). */
export function NewServerPage() {
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-4">
      <RequireAdmin what="remote MCP servers">
        <NewServerContent />
      </RequireAdmin>
    </div>
  )
}

function NewServerContent() {
  const { org } = useOrg()
  const actions = useMcpActions(org.orgId)
  const navigate = useNavigate()
  const saved = useRef(false)
  return (
    <>
      <PageHeader
        title="Connect a remote server"
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to="/mcp-servers">MCP servers</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>Connect</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        description={`Koinon reaches the server through the gateway and lists its tools. Each new tool waits for an admin review before agents get it. The credential reaches the gateway in about ${availabilityMinutes} minutes.`}
      />
      <ServerForm
        actions={actions}
        // ServerForm calls onSaved, then onDone: after a save, onDone must not undo the move to the new server.
        onDone={() => {
          if (!saved.current) void navigate({ to: '/mcp-servers' })
        }}
        onSaved={(name, serverId) => {
          saved.current = true
          toast(`Connected server ${name}. Its tools show on its page after the first sync.`)
          void navigate(serverId ? { to: '/mcp-servers/$serverId', params: { serverId } } : { to: '/mcp-servers' })
        }}
      />
    </>
  )
}
