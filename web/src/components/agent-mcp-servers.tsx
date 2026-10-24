// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Health, McpServerService, type Server, type Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { useMcpActions } from '@/hooks/use-mcp-actions'
import { allowStateFrom, allowlistProblem, buildAllowlist, healthLabel, tierExplanation, tierLabel, toolsOf, type AllowState } from '@/lib/mcp-servers'
import { useOrg } from '@/lib/org-context'

/** AgentMcpServers is the admin's allowlist editor: which remote MCP servers (and tools) an agent may use. */
export function AgentMcpServers({ agentId }: { agentId: string }) {
  const { org } = useOrg()
  const orgId = org.orgId
  const servers = useQuery(McpServerService.method.listServers, { orgId })
  const tools = useQuery(McpServerService.method.listTools, { orgId, serverId: '' })
  const allowed = useQuery(McpServerService.method.getAgentServers, { orgId, agentId })
  const failed = servers.error ?? tools.error ?? allowed.error
  return (
    <section aria-labelledby="agent-mcp" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <h2 id="agent-mcp" className="font-semibold">
        MCP servers
      </h2>
      {failed ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(failed).rawMessage}</AlertDescription>
        </Alert>
      ) : !servers.data || !tools.data || !allowed.data ? (
        <p role="status">Loading MCP servers…</p>
      ) : (
        <Editor
          agentId={agentId}
          servers={servers.data.servers}
          tools={tools.data.tools}
          initial={allowStateFrom(allowed.data.servers)}
        />
      )}
    </section>
  )
}

function Editor({ agentId, servers, tools, initial }: { agentId: string; servers: Server[]; tools: Tool[]; initial: Record<string, AllowState> }) {
  const { org } = useOrg()
  const actions = useMcpActions(org.orgId)
  const [state, setState] = useState(initial)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const [saved, setSaved] = useState(false)
  const problem = allowlistProblem(servers, state)

  const change = (id: string, p: Partial<AllowState>) => {
    setSaved(false)
    setState((cur) => ({ ...cur, [id]: { ...(cur[id] ?? { allowed: false, limited: false, tools: [] }), ...p } }))
  }

  async function save() {
    if (problem) return
    setPending(true)
    setError(undefined)
    try {
      await actions.setAgentServers(agentId, buildAllowlist(servers, state))
      setSaved(true)
    } catch (e) {
      setError(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }

  if (servers.length === 0) {
    return (
      <p className="text-muted-foreground">
        The org has no remote MCP servers. Add one on the{' '}
        <Link to="/servers" className="underline">
          Servers page
        </Link>
        .
      </p>
    )
  }
  return (
    <>
      <p className="text-muted-foreground">
        Choose the servers this agent may use. By default it gets the whole server, including tools the server adds later, which start in the Ask tier. {tierExplanation}
      </p>
      <ul className="grid gap-3" aria-label="MCP servers of this agent">
        {servers.map((s) => {
          const st = state[s.id]
          const own = toolsOf(tools, s.id).filter((t) => t.present)
          return (
            <li key={s.id} className="grid gap-2 rounded-md border p-3">
              <div className="flex flex-wrap items-center gap-2">
                <label className="flex items-center gap-2 font-medium">
                  <input type="checkbox" checked={!!st?.allowed} onChange={(e) => change(s.id, { allowed: e.target.checked })} />
                  <span>
                    Allow <span className="font-mono">{s.name}</span>
                  </span>
                </label>
                <Badge variant={s.health === Health.UNHEALTHY ? 'destructive' : 'outline'}>{healthLabel(s.health)}</Badge>
              </div>
              {s.health === Health.UNHEALTHY && <p className="text-destructive text-xs">{s.name} is unhealthy. Its tools are withdrawn until it recovers.</p>}
              {st?.allowed && (
                <div className="grid gap-2 pl-6">
                  <label className="flex items-center gap-2">
                    <input type="checkbox" checked={st.limited} onChange={(e) => change(s.id, { limited: e.target.checked })} />
                    Limit to some tools
                  </label>
                  {!st.limited && <p className="text-muted-foreground text-xs">The whole server, including tools it adds later.</p>}
                  {st.limited && (
                    <fieldset className="grid gap-1">
                      <legend className="text-muted-foreground mb-1 text-xs">Tools of {s.name} this agent may use</legend>
                      {own.length === 0 && <p className="text-muted-foreground text-xs">The server lists no tools yet.</p>}
                      {own.map((t) => (
                        <label key={t.name} className="flex items-center gap-2">
                          <input
                            type="checkbox"
                            checked={st.tools.includes(t.name)}
                            onChange={(e) => change(s.id, { tools: e.target.checked ? [...st.tools, t.name] : st.tools.filter((n) => n !== t.name) })}
                          />
                          <span className="font-mono break-all">{t.name}</span>
                          <span className="text-muted-foreground text-xs">{tierLabel(t.tier)}</span>
                        </label>
                      ))}
                    </fieldset>
                  )}
                </div>
              )}
            </li>
          )
        })}
      </ul>
      {problem && (
        <p role="alert" className="text-destructive text-xs">
          {problem}
        </p>
      )}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {error}</AlertDescription>
        </Alert>
      )}
      {saved && <p role="status">Saved.</p>}
      <div>
        <Button disabled={pending || !!problem} onClick={() => void save()}>
          Save MCP servers
        </Button>
      </div>
    </>
  )
}
