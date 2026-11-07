// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { Link } from '@tanstack/react-router'
import { TriangleAlert, CircleCheck, FileText } from 'lucide-react'
import { useId, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { FileStatus, ScopeKind, Status, type Approval, type FileChange, type FileDiff, type GetApprovalDiffResponse, type LeakGuard, type McpCall, type SavedWorkCall } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { approvalStatusLabel, approveLabel, defaultScopeEnd, endOfLocalDay, localDate, targetRepo, toolChanged, toolVersion, type ScopeChoice } from '@/lib/approvals'
import { ciPathsHit } from '@/lib/ci-paths'
import { formatBytes, riskFlagsFor, riskItems, type RiskItem } from '@/lib/risk'
import { cn } from '@/lib/utils'

export type ApprovalCardProps = {
  approval: Approval
  /** Whether the viewer may decide: an org admin or owner, and the API agrees. */
  canDecide: boolean
  /** The scope is only passed when it is wider than once. */
  onApprove?: (note: string, scope?: ScopeChoice) => void
  onDeny?: (note: string) => void
  pending?: boolean
  error?: string
  /**
   * The approval was refused because the request changed since the card
   * loaded (Aborted, HTTP 409): the viewer must reload and review it again.
   */
  changed?: boolean
  onReload?: () => void
  /** The change against the base branch, from GetApprovalDiff. */
  diff?: GetApprovalDiffResponse
  diffPending?: boolean
  /** Why GetApprovalDiff failed; the files then show their full new content. */
  diffError?: string
  /** Link to the standalone view (shown in the ticket timeline). */
  showPermalink?: boolean
  /** The signed-in user's ID, to stop the first of two approvers from giving the second approval. */
  currentUserId?: string
}

/**
 * ApprovalCard shows one ask-tier call (ADR 0049): push-and-run risk first,
 * then what the call does and where, then the change itself, collapsed.
 * Approving binds to the args_sha256 the card was loaded with.
 */
export function ApprovalCard({
  approval: a,
  canDecide,
  onApprove,
  onDeny,
  pending,
  error,
  changed,
  onReload,
  diff,
  diffPending,
  diffError,
  showPermalink,
  currentUserId,
}: ApprovalCardProps) {
  const headingId = useId()
  const pr = a.call.case === 'pullRequest' ? a.call.value : undefined
  const comment = a.call.case === 'comment' ? a.call.value : undefined
  const saved = a.call.case === 'savedWork' ? a.call.value : undefined
  const savedClosed = !!saved && saved.status !== 'saved'
  const mcp = a.connector === 'mcp'
  const mcpCall = a.call.case === 'mcp' ? a.call.value : undefined
  const stale = !!mcpCall && toolChanged(mcpCall.contentHash, mcpCall.currentContentHash)
  const two = a.approvalsRequired === 2
  const isFirstApprover = two && !!a.firstApprover && a.firstApprover === currentUserId
  const blocked = stale
    ? 'The tool changed after this request; Koinon will not run it.'
    : isFirstApprover
      ? 'You gave the first approval; a different admin or owner must give the second.'
      : undefined
  const items = riskItems(a.risk)
  const isPending = a.status === Status.PENDING
  const resultUrl = typeof a.result?.url === 'string' ? a.result.url : undefined

  return (
    <article aria-labelledby={headingId} data-testid="approval-card" className="bg-card grid gap-4 rounded-lg border p-4 shadow-xs">
      <header className="flex flex-wrap items-center gap-2">
        <h3 id={headingId} className="font-semibold">
          {saved ? (
            'Push saved work to a branch'
          ) : (
            <>
              Approve <code className="font-mono text-sm">{a.tool}</code> on {targetRepo(a.target)}?
            </>
          )}
        </h3>
        <Badge variant={isPending ? 'default' : 'secondary'} className="ml-auto">
          {approvalStatusLabel(a.status)}
        </Badge>
      </header>

      {pr && <RiskSummary items={items} />}

      {saved && <SavedWork call={saved} ciPaths={a.ciPaths} ciFoldCase={a.ciFoldCase} />}

      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Tool</dt>
        <dd className="font-mono">{a.tool}</dd>
        <dt className="text-muted-foreground">{mcp ? 'Server' : 'Repository'}</dt>
        <dd>{saved?.repository || targetRepo(a.target)}</dd>
        {pr && (
          <>
            <dt className="text-muted-foreground">Branch</dt>
            <dd className="font-mono">
              {pr.branch} → {pr.base || 'default branch'}
            </dd>
            <dt className="text-muted-foreground">Title</dt>
            <dd>{pr.title}</dd>
          </>
        )}
        {comment && (
          <>
            <dt className="text-muted-foreground">Issue</dt>
            <dd>#{comment.issueNumber.toString()}</dd>
          </>
        )}
      </dl>

      {pr?.body && (
        <section aria-label="Pull request body">
          <h4 className="text-muted-foreground mb-1 text-xs font-medium uppercase">Body</h4>
          <p className="bg-muted/40 max-h-48 overflow-auto rounded-md border p-2 text-sm whitespace-pre-wrap">{pr.body}</p>
        </section>
      )}
      {comment && (
        <section aria-label="Comment">
          <h4 className="text-muted-foreground mb-1 text-xs font-medium uppercase">Comment</h4>
          <p className="bg-muted/40 rounded-md border p-2 text-sm whitespace-pre-wrap">{comment.body}</p>
        </section>
      )}

      {two && (
        <section aria-label="Approvers" className="rounded-md border p-3 text-sm">
          <p className="font-medium">Needs two approvers</p>
          {isPending && a.firstApprover && <p className="text-muted-foreground">First approval given; a second person (not the first approver, not the task&apos;s creator) must approve.</p>}
        </section>
      )}

      {mcpCall && <McpToolInfo call={mcpCall} stale={stale} />}

      {mcp && <McpArguments json={a.argsJson} />}

      {pr && <FileList files={pr.files} items={items} diff={diff} diffPending={diffPending} diffError={diffError} />}

      {a.scopeId && (
        <p role="note" className="bg-muted/30 rounded-md border p-2 text-sm" data-testid="scope-note">
          Ran without a new decision: allowed by a scope you or another admin granted ({a.scopeKind === ScopeKind.TASK ? 'this task' : 'this agent'}).
        </p>
      )}

      <Outcome approval={a} resultUrl={resultUrl} />

      {isPending &&
        !savedClosed &&
        (canDecide ? (
          <DecisionForm
            onApprove={onApprove}
            onDeny={onDeny}
            pending={pending}
            sha={a.argsSha256}
            approveText={approveLabel(a.approvalsRequired, a.firstApprover)}
            blocked={blocked}
            canScope={!two && !a.system && !saved}
            agentTool={`${a.tool} on ${saved?.repository || targetRepo(a.target)}`}
          />
        ) : (
          <p className="text-muted-foreground text-sm">Only org admins and owners can approve or deny this request.</p>
        ))}
      {changed && (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-2">
            <span>This request changed since you opened it. Reload to review the new version.</span>
            <Button size="sm" variant="outline" className="ml-auto" onClick={onReload}>
              Reload
            </Button>
          </AlertDescription>
        </Alert>
      )}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {showPermalink && (
        <Link to="/approvals/$approvalId" params={{ approvalId: a.id }} className="text-muted-foreground text-xs hover:underline">
          Open this request on its own page
        </Link>
      )}
    </article>
  )
}

const savedStatusText: Record<string, string> = { A: 'Added', M: 'Modified', D: 'Deleted' }

/**
 * SavedWork shows what Koinon saved from a sandbox it had to recreate: a diffstat of the files, where they would
 * go, and until when they are kept. The files are not shown (there is no diff): approving pushes them to a new
 * branch, never the default branch.
 */
function SavedWork({ call: w, ciPaths, ciFoldCase }: { call: SavedWorkCall; ciPaths: readonly string[]; ciFoldCase: boolean }) {
  const ciHit = ciPathsHit(
    w.files.map((f) => f.path),
    ciPaths,
    ciFoldCase,
  )
  const added = w.files.reduce((n, f) => n + f.added, 0)
  const deleted = w.files.reduce((n, f) => n + f.deleted, 0)
  const expires = w.expiresAt ? timestampDate(w.expiresAt) : undefined
  return (
    <section aria-label="Saved work" className="grid gap-3 text-sm">
      <p role="note" className="bg-muted/30 rounded-md border p-3">
        Koinon saved this uncommitted work from a sandbox that had to be recreated. Nothing is pushed until you approve. It goes to a new branch, never the default branch.
      </p>
      {ciHit.length > 0 && (
        <p role="note" className="rounded-md border border-amber-600 p-3" data-testid="workflow-warning">
          This work changes files under {ciHit.join(', ')}, where this code host runs CI. The host may refuse the push of workflow files with this connection. A pushed branch can also start existing
          workflows that run on push, and those can use the repository&apos;s secrets, so read these files before you approve.
        </p>
      )}
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        <dt className="text-muted-foreground">Branch</dt>
        <dd className="font-mono break-all">{w.branch}</dd>
        {w.base && (
          <>
            <dt className="text-muted-foreground">Base commit</dt>
            <dd className="font-mono" title={w.base}>
              {w.base.slice(0, 7)}
            </dd>
          </>
        )}
        <dt className="text-muted-foreground">Size</dt>
        <dd>{formatBytes(Number(w.sizeBytes))}</dd>
        <dt className="text-muted-foreground">Saved work</dt>
        <dd>{w.status === 'expired' ? 'Expired' : w.status === 'pushed' ? 'Pushed' : 'Saved'}</dd>
        {expires && (
          <>
            <dt className="text-muted-foreground">{w.status === 'expired' ? 'Expired' : 'Kept until'}</dt>
            <dd>
              <time dateTime={expires.toISOString()}>{expires.toLocaleString()}</time>
            </dd>
          </>
        )}
      </dl>
      {w.status === 'expired' && <p role="note">This saved work has expired and was deleted. It can no longer be pushed.</p>}
      {w.status === 'pushed' && <p role="note">This saved work has been pushed to the branch.</p>}
      <div>
        <h4 className="text-muted-foreground mb-1 text-xs font-medium uppercase">Files ({w.files.length})</h4>
        <table className="w-full text-left" aria-label="Saved files">
          <thead>
            <tr className="text-muted-foreground border-b text-xs">
              <th scope="col" className="py-1 pr-2 font-medium">
                File
              </th>
              <th scope="col" className="px-2 py-1 font-medium">
                Change
              </th>
              <th scope="col" className="py-1 pl-2 text-right font-medium">
                Lines
              </th>
            </tr>
          </thead>
          <tbody>
            {w.files.map((f) => (
              <tr key={f.path} className="border-b last:border-b-0">
                <td className="py-1 pr-2 font-mono break-all">{f.path}</td>
                <td className="px-2 py-1 whitespace-nowrap">
                  {f.status} <span className="text-muted-foreground">({savedStatusText[f.status] ?? 'Changed'})</span>
                </td>
                <td className="py-1 pl-2 text-right font-mono whitespace-nowrap">
                  {f.binary ? (
                    <span className="text-muted-foreground">binary, {formatBytes(Number(f.size))}</span>
                  ) : (
                    <>
                      +{f.added} -{f.deleted}
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
          <tfoot>
            <tr className="border-t font-medium">
              <th scope="row" colSpan={2} className="py-1 pr-2 text-left font-medium">
                Total, {w.files.length} {w.files.length === 1 ? 'file' : 'files'}
              </th>
              <td className="py-1 pl-2 text-right font-mono whitespace-nowrap">
                +{added} -{deleted}
              </td>
            </tr>
          </tfoot>
        </table>
      </div>
      {w.skipped.length > 0 && (
        <div>
          <h4 className="text-muted-foreground mb-1 text-xs font-medium uppercase">Left out, as reported by the sandbox (not verified)</h4>
          <ul className="list-disc pl-5">
            {w.skipped.map((n, i) => (
              <li key={i}>{n}</li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

/** prettyJson indents canonical JSON for reading; text that does not parse is shown as it is. */
function prettyJson(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

/** McpToolInfo shows which version of the tool the request was made against, and whether it has since changed. */
function McpToolInfo({ call, stale }: { call: McpCall; stale: boolean }) {
  return (
    <section aria-label="Tool version" className="grid gap-2 text-sm">
      <p>
        Tool version <code className="font-mono">{toolVersion(call.contentHash)}</code>
      </p>
      {stale && (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="font-medium">The tool changed after this request; Koinon will not run it. Deny it and let the agent ask again.</AlertDescription>
        </Alert>
      )}
      {call.reviewReason && (
        <p role="note" className="rounded-md border border-amber-500/60 bg-amber-500/5 p-2">
          {call.reviewReason}
        </p>
      )}
      {call.leakGuard && <LeakGuardNote guard={call.leakGuard} />}
      {call.toolDescription && (
        <details className="rounded-md border" data-testid="mcp-tool-description">
          <summary className="cursor-pointer px-2 py-1 font-medium">What the tool says it does</summary>
          <p className="bg-muted/40 max-h-48 overflow-auto border-t p-2 whitespace-pre-wrap">{call.toolDescription}</p>
        </details>
      )}
    </section>
  )
}

/** LeakGuardNote says why the leak guard (ADR 0091) put this call on a card: which legs the session holds and where the call goes. */
function LeakGuardNote({ guard }: { guard: LeakGuard }) {
  const legs = [guard.untrustedInput && 'untrusted input', guard.privateData && 'private data'].filter(Boolean)
  const destination = Object.entries(guard.destination)
  return (
    <section aria-label="Leak guard" data-testid="leak-guard" className="rounded-md border border-amber-500/60 bg-amber-500/5 p-2">
      <h4 className="mb-1 font-semibold">Held by the leak guard</h4>
      <p>
        This session holds <span data-testid="leak-guard-legs">{legs.length ? legs.join(' and ') : 'no leg'}</span>
        {guard.sinkClass ? <> and the tool is a {guard.sinkClass} sink.</> : '.'}
      </p>
      {destination.length > 0 && (
        <ul aria-label="Destination" className="font-mono text-xs">
          {destination.map(([k, v]) => (
            <li key={k}>
              {k}: {v}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

/** McpArguments shows the arguments an MCP call is approved with; the approval binds to their hash. */
function McpArguments({ json }: { json: string }) {
  return (
    <section aria-label="Arguments">
      <details className="rounded-md border" data-testid="mcp-arguments">
        <summary className="cursor-pointer px-2 py-1 text-sm font-medium">Arguments</summary>
        <pre className="bg-muted/40 max-h-96 overflow-auto border-t p-2 font-mono text-xs" aria-label="Call arguments">
          {json ? prettyJson(json) : '(none)'}
        </pre>
      </details>
    </section>
  )
}

function RiskSummary({ items }: { items: readonly RiskItem[] }) {
  const flagged = items.filter((i) => i.flagged)
  return (
    <section aria-label="Push and run risk" className={cn('rounded-md border p-3', flagged.length ? 'border-amber-500/60 bg-amber-500/5' : 'bg-muted/30')}>
      <h4 className="mb-1 text-sm font-semibold">{flagged.length ? 'Check before approving' : 'No push-and-run risk found'}</h4>
      <p className="text-muted-foreground mb-2 text-xs">
        Approving lets Koinon push this branch and open the pull request; CI may run it with the repository’s secrets before anyone reviews it on GitHub.
      </p>
      <ul className="grid gap-1.5 text-sm" aria-label="Risks">
        {items.map((i) => (
          <li key={i.key} data-risk={i.key} data-flagged={i.flagged} className="flex gap-2">
            {i.flagged ? (
              <TriangleAlert aria-hidden className="mt-0.5 size-4 shrink-0 text-amber-600" />
            ) : (
              <CircleCheck aria-hidden className="text-muted-foreground mt-0.5 size-4 shrink-0" />
            )}
            <div>
              <span className={cn(i.flagged ? 'font-medium' : 'text-muted-foreground')}>
                {i.label}
                {i.key === 'size' ? `: ${i.why}` : i.paths.length ? ` (${i.paths.length})` : ': none'}
              </span>
              {i.key !== 'size' && i.flagged && <span className="text-muted-foreground"> · {i.why}</span>}
              {i.paths.length > 0 && (
                <ul className="font-mono text-xs">
                  {i.paths.map((p) => (
                    <li key={p}>{p}</li>
                  ))}
                </ul>
              )}
            </div>
          </li>
        ))}
      </ul>
    </section>
  )
}

const statusLabel: Partial<Record<FileStatus, string>> = {
  [FileStatus.ADDED]: 'added',
  [FileStatus.MODIFIED]: 'modified',
  [FileStatus.DELETED]: 'deleted',
}

function FileList({
  files,
  items,
  diff,
  diffPending,
  diffError,
}: {
  files: readonly FileChange[]
  items: readonly RiskItem[]
  diff?: GetApprovalDiffResponse
  diffPending?: boolean
  diffError?: string
}) {
  const byPath = new Map((diff?.files ?? []).map((d) => [d.path, d]))
  const binary = new Set(items.find((i) => i.key === 'binary')?.paths ?? [])
  const base = diff?.base || 'the base branch'
  return (
    <section aria-label="Files">
      <h4 className="text-muted-foreground mb-1 text-xs font-medium uppercase">Files ({files.length})</h4>
      <p className="text-muted-foreground mb-2 text-xs">
        {diffPending
          ? 'Comparing with the base branch on GitHub…'
          : diff
            ? `Each file shows its diff against ${base} as it is now.`
            : `Could not compare with the base branch${diffError ? ` (${diffError})` : ''}; each file shows its full new content.`}
      </p>
      <ul className="grid gap-1">
        {files.map((f) => {
          const flags = riskFlagsFor(f.path, items).filter((i) => i.key !== 'deletions')
          const d = byPath.get(f.path)
          const status = f.delete ? 'deleted' : d && statusLabel[d.status]
          return (
            <li key={f.path}>
              <details className="rounded-md border" data-testid="file-diff">
                <summary className="flex cursor-pointer flex-wrap items-center gap-2 px-2 py-1 text-sm">
                  <FileText aria-hidden className="text-muted-foreground size-4" />
                  <span className="font-mono">{f.path}</span>
                  {status && <Badge variant={status === 'deleted' ? 'destructive' : 'secondary'}>{status}</Badge>}
                  {flags.map((i) => (
                    <Badge key={i.key} variant="outline">
                      {i.label.toLowerCase()}
                    </Badge>
                  ))}
                  {!f.delete && <span className="text-muted-foreground ml-auto text-xs">{formatBytes(new TextEncoder().encode(f.content).length)}</span>}
                </summary>
                <FileBody file={f} diff={d} binary={(d?.binary ?? false) || binary.has(f.path)} base={base} compared={!!diff} />
              </details>
            </li>
          )
        })}
      </ul>
    </section>
  )
}

/** modeChange is the git mode line of a file, as git prints it: a modified file whose mode changes, or a new executable file. */
function modeChange(d?: FileDiff): string | undefined {
  if (!d?.newMode) return undefined
  if (d.oldMode && d.oldMode !== d.newMode) return `mode ${d.oldMode} → ${d.newMode}`
  if (!d.oldMode && d.newMode !== '100644') return `new file mode ${d.newMode}`
  return undefined
}

function FileBody(props: { file: FileChange; diff?: FileDiff; binary: boolean; base: string; compared: boolean }) {
  const mode = modeChange(props.diff)
  return (
    <>
      {mode && (
        <p className="border-t px-2 py-1 font-mono text-xs" data-testid="mode-change">
          {mode}
        </p>
      )}
      <FileContent {...props} modeChanged={!!mode} />
    </>
  )
}

function FileContent({
  file: f,
  diff: d,
  binary,
  base,
  compared,
  modeChanged,
}: {
  file: FileChange
  diff?: FileDiff
  binary: boolean
  base: string
  compared: boolean
  modeChanged: boolean
}) {
  if (binary) return <p className="text-muted-foreground border-t px-2 py-1 text-sm">Binary file; its content is not shown.</p>
  if (d?.unifiedDiff) return <UnifiedDiff path={f.path} text={d.unifiedDiff} />
  if (d && !d.fallbackReason && d.status !== FileStatus.UNSPECIFIED) {
    return <p className="text-muted-foreground border-t px-2 py-1 text-sm">{modeChanged ? `Content unchanged against ${base}.` : `No change against ${base}.`}</p>
  }
  return (
    <>
      {compared && (
        <p className="text-muted-foreground border-t px-2 py-1 text-xs" role="note">
          No diff{d?.fallbackReason ? `: ${d.fallbackReason}` : ''}. Showing the full new content.
        </p>
      )}
      {f.delete ? (
        <p className="text-muted-foreground border-t px-2 py-1 text-sm">This file is deleted.</p>
      ) : (
        <pre className="bg-muted/40 max-h-96 overflow-auto border-t p-2 text-xs" aria-label={`New content of ${f.path}`}>
          {f.content}
        </pre>
      )}
    </>
  )
}

/** UnifiedDiff colours a unified diff line by line; the file headers are dropped. */
function UnifiedDiff({ path, text }: { path: string; text: string }) {
  const lines = text.replace(/\n$/, '').split('\n')
  return (
    <pre className="max-h-96 overflow-auto border-t py-1 text-xs" aria-label={`Diff of ${path}`}>
      {lines.map((line, i) => {
        if (i < 2 && (line.startsWith('--- ') || line.startsWith('+++ '))) return null
        const kind = line.startsWith('@@') ? 'hunk' : line.startsWith('+') ? 'add' : line.startsWith('-') ? 'del' : line.startsWith('\\') ? 'note' : 'ctx'
        return (
          <span
            key={i}
            data-line={kind}
            className={cn(
              'block px-2 whitespace-pre',
              kind === 'add' && 'bg-emerald-500/15',
              kind === 'del' && 'bg-red-500/15',
              (kind === 'hunk' || kind === 'note') && 'text-muted-foreground',
            )}
          >
            {line || ' '}
          </span>
        )
      })}
    </pre>
  )
}

function Outcome({ approval: a, resultUrl }: { approval: Approval; resultUrl?: string }) {
  if (a.status === Status.PENDING) return null
  return (
    <div className="text-sm" role="status">
      {approvalStatusLabel(a.status)}
      {a.decisionNote && <>: {a.decisionNote}</>}
      {resultUrl && (
        <>
          {' · '}
          <a href={resultUrl} target="_blank" rel="noreferrer" className="underline">
            {resultUrl}
          </a>
        </>
      )}
      {a.error && <span className="text-destructive"> · {a.error}</span>}
    </div>
  )
}

const DAY = 24 * 3600 * 1000

function DecisionForm({
  onApprove,
  onDeny,
  pending,
  sha,
  approveText,
  blocked,
  canScope,
  agentTool,
}: {
  onApprove?: (note: string, scope?: ScopeChoice) => void
  onDeny?: (note: string) => void
  pending?: boolean
  sha: string
  approveText: string
  blocked?: string
  /** Whether a scope wider than once can be offered (not for two approvers or Koinon's own requests). */
  canScope?: boolean
  agentTool?: string
}) {
  const noteId = useId()
  const blockedId = useId()
  const scopeId = useId()
  const untilId = useId()
  const [note, setNote] = useState('')
  const [scope, setScope] = useState<'once' | 'task' | 'agent'>('once')
  const [until, setUntil] = useState(() => defaultScopeEnd())
  // The form lives for one decision; the clock it checks the date against is read when it opens.
  const [openedAt] = useState(() => Date.now())
  const end = endOfLocalDay(until)
  const minDate = localDate(new Date(openedAt + DAY))
  // 364 days keeps 23:59:59 of the last day inside the server's 365-day limit in every time zone.
  const maxDate = localDate(new Date(openedAt + 364 * DAY))
  const badDate = scope === 'agent' && (!end || until < minDate || until > maxDate)
  const approve = () => {
    const n = note.trim()
    if (scope === 'once' || !canScope) onApprove?.(n)
    else if (scope === 'task') onApprove?.(n, { kind: 'task' })
    else if (end) onApprove?.(n, { kind: 'agent', until: end })
  }
  return (
    <div className="grid gap-2 border-t pt-3">
      <Label htmlFor={noteId}>Note (optional)</Label>
      <Textarea id={noteId} rows={2} value={note} onChange={(e) => setNote(e.target.value)} placeholder="Shown on the task; on denial the agent reads it." />
      {canScope && (
        <div className="grid gap-2" data-testid="scope-choice">
          <Label htmlFor={scopeId}>How far does this approval reach?</Label>
          <NativeSelect id={scopeId} value={scope} onChange={(e) => setScope(e.target.value as 'once' | 'task' | 'agent')}>
            <option value="once">Only this call</option>
            <option value="task">This task: later {agentTool ?? 'calls like this'}, for this task (at most 30 days)</option>
            <option value="agent">This agent: later {agentTool ?? 'calls like this'}, until a date</option>
          </NativeSelect>
          {scope === 'agent' && (
            <div className="grid gap-1">
              <Label htmlFor={untilId}>Until (end of day)</Label>
              <Input id={untilId} type="date" min={minDate} max={maxDate} value={until} onChange={(e) => setUntil(e.target.value)} aria-invalid={badDate} className="max-w-48" />
              {badDate && <p className="text-destructive text-sm">Choose a date from tomorrow to a year ahead.</p>}
            </div>
          )}
          {scope !== 'once' && (
            <p role="note" className="text-muted-foreground text-xs">
              Same tool, same repository or server, arguments may differ. It shows on the agent&apos;s page, is audited and can be revoked. Changes to CI files, scripts,
              dependencies, binary or executable files always ask again.
            </p>
          )}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground text-xs" title={sha}>
          Approves exactly this change (sha256 {sha.slice(0, 12)}…)
        </span>
        <Button variant="outline" className="ml-auto" disabled={pending} onClick={() => onDeny?.(note.trim())}>
          Deny
        </Button>
        <Button disabled={pending || !!blocked || badDate} aria-describedby={blocked ? blockedId : undefined} onClick={approve}>
          {approveText}
        </Button>
      </div>
      {blocked && (
        <p id={blockedId} className="text-muted-foreground text-sm">
          {blocked}
        </p>
      )}
    </div>
  )
}
