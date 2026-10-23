// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { TriangleAlert, CircleCheck, FileText } from 'lucide-react'
import { useId, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { FileStatus, Status, type Approval, type FileChange, type FileDiff, type GetApprovalDiffResponse } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { approvalStatusLabel, targetRepo } from '@/lib/approvals'
import { formatBytes, riskFlagsFor, riskItems, type RiskItem } from '@/lib/risk'
import { cn } from '@/lib/utils'

export type ApprovalCardProps = {
  approval: Approval
  /** Whether the viewer may decide: an org admin or owner, and the API agrees. */
  canDecide: boolean
  onApprove?: (note: string) => void
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
}: ApprovalCardProps) {
  const headingId = useId()
  const pr = a.call.case === 'pullRequest' ? a.call.value : undefined
  const comment = a.call.case === 'comment' ? a.call.value : undefined
  const items = riskItems(a.risk)
  const isPending = a.status === Status.PENDING
  const resultUrl = typeof a.result?.url === 'string' ? a.result.url : undefined

  return (
    <article aria-labelledby={headingId} data-testid="approval-card" className="bg-card grid gap-4 rounded-lg border p-4 shadow-xs">
      <header className="flex flex-wrap items-center gap-2">
        <h3 id={headingId} className="font-semibold">
          Approve <code className="font-mono text-sm">{a.tool}</code> on {targetRepo(a.target)}?
        </h3>
        <Badge variant={isPending ? 'default' : 'secondary'} className="ml-auto">
          {approvalStatusLabel(a.status)}
        </Badge>
      </header>

      {pr && <RiskSummary items={items} />}

      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Tool</dt>
        <dd className="font-mono">{a.tool}</dd>
        <dt className="text-muted-foreground">Repository</dt>
        <dd>{targetRepo(a.target)}</dd>
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

      {pr && <FileList files={pr.files} items={items} diff={diff} diffPending={diffPending} diffError={diffError} />}

      <Outcome approval={a} resultUrl={resultUrl} />

      {isPending &&
        (canDecide ? (
          <DecisionForm onApprove={onApprove} onDeny={onDeny} pending={pending} sha={a.argsSha256} />
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

function DecisionForm({ onApprove, onDeny, pending, sha }: { onApprove?: (note: string) => void; onDeny?: (note: string) => void; pending?: boolean; sha: string }) {
  const noteId = useId()
  const [note, setNote] = useState('')
  return (
    <div className="grid gap-2 border-t pt-3">
      <Label htmlFor={noteId}>Note (optional)</Label>
      <Textarea id={noteId} rows={2} value={note} onChange={(e) => setNote(e.target.value)} placeholder="Shown on the ticket; on denial the agent reads it." />
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground text-xs" title={sha}>
          Approves exactly this change (sha256 {sha.slice(0, 12)}…)
        </span>
        <Button variant="outline" className="ml-auto" disabled={pending} onClick={() => onDeny?.(note.trim())}>
          Deny
        </Button>
        <Button disabled={pending} onClick={() => onApprove?.(note.trim())}>
          Approve
        </Button>
      </div>
    </div>
  )
}
