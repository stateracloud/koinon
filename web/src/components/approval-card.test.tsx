// SPDX-License-Identifier: Apache-2.0

import { localDate } from '@/lib/approvals'
import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { timestampDate } from '@bufbuild/protobuf/wkt'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import {
  ApprovalSchema,
  ApprovalService,
  Decision,
  FileChangeSchema,
  FileStatus,
  GetApprovalDiffResponseSchema,
  McpCallSchema,
  PullRequestCallSchema,
  RiskSchema,
  ScopeKind,
  Status,
  type ApproveRequest,
  type DenyRequest,
} from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { riskItems } from '@/lib/risk'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const sha = 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'

type McpInit = { contentHash?: string; currentContentHash?: string; reviewReason?: string; toolDescription?: string; approvalsRequired?: number; firstApprover?: string; errorOnApprove?: boolean; leakGuard?: { untrustedInput: boolean; privateData: boolean } }
let mcpInit: McpInit = {}

function mcpApproval() {
  return create(ApprovalSchema, {
    approvalsRequired: mcpInit.approvalsRequired ?? 1,
    firstApprover: mcpInit.firstApprover ?? '',
    call: {
      case: 'mcp',
      value: create(McpCallSchema, {
        serverId: 's-docs',
        serverName: 'docs',
        contentHash: mcpInit.contentHash ?? 'abcdef0123456789',
        currentContentHash: mcpInit.currentContentHash ?? '',
        reviewReason: mcpInit.reviewReason ?? '',
        toolDescription: mcpInit.toolDescription ?? '',
        leakGuard: mcpInit.leakGuard && {
          ...mcpInit.leakGuard,
          reasons: ['both_legs'],
          sinkClass: 'external-high',
          destination: { url: 'https://attacker.invalid/x' },
        },
      }),
    },
    id: 'ap-1',
    orgId,
    ticketId: 't-1',
    runId: 'r-1',
    connector: 'mcp',
    tool: 'create_page',
    target: 'mcp:docs',
    argsJson: '{"title":"Hello","tags":["a","b"]}',
    argsSha256: sha,
    decision: Decision.ASK,
    status: Status.PENDING,
    canDecide: true,
  })
}

function approval(opts: { canDecide?: boolean; status?: Status; executable?: boolean; mcp?: boolean } = {}) {
  if (opts.mcp) return mcpApproval()
  return create(ApprovalSchema, {
    id: 'ap-1',
    orgId,
    ticketId: 't-1',
    runId: 'r-1',
    connector: 'github',
    tool: 'create_pull_request',
    target: 'github:acme/widgets',
    argsSha256: sha,
    decision: Decision.ASK,
    status: opts.status ?? Status.PENDING,
    canDecide: opts.canDecide ?? true,
    risk: create(RiskSchema, {
      files: 4n,
      bytes: 120n,
      deletions: ['old.txt'],
      ci: ['.github/dependabot.yml'],
      scripts: ['scripts/release.sh'],
      dependencies: ['package.json'],
      binary: [],
      executable: opts.executable ? ['scripts/release.sh'] : [],
    }),
    call: {
      case: 'pullRequest',
      value: create(PullRequestCallSchema, {
        branch: 'koinon/fix-typo',
        title: 'Fix a typo in the README',
        body: 'Fixes "recieve".',
        files: [
          create(FileChangeSchema, { path: 'README.md', content: '# Widgets\nreceive\n' }),
          create(FileChangeSchema, { path: '.github/dependabot.yml', content: 'version: 2\n' }),
          create(FileChangeSchema, { path: 'scripts/release.sh', content: '#!/bin/sh\necho hi\n', executable: opts.executable ?? false }),
          create(FileChangeSchema, { path: 'old.txt', delete: true }),
        ],
      }),
    },
  })
}

const changedSha = 'a'.repeat(64)

type DiffInit = MessageInitShape<typeof GetApprovalDiffResponseSchema>

function setup(opts: { mcpInit?: McpInit; role?: OrgRole; canDecide?: boolean; changed?: boolean; diff?: DiffInit; executable?: boolean; mcp?: boolean } = {}) {
  mcpInit = opts.mcpInit ?? {}
  const approves: ApproveRequest[] = []
  const denies: DenyRequest[] = []
  let current = approval({ canDecide: opts.canDecide, executable: opts.executable, mcp: opts.mcp })
  let gets = 0
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ApprovalService, {
      getApproval: () => {
        gets++
        return { approval: current }
      },
      getApprovalDiff: () => {
        if (!opts.diff) throw new ConnectError('approvals are not configured on this server', Code.Unavailable)
        return opts.diff
      },
      approve: (req) => {
        approves.push(req)
        if (mcpInit.errorOnApprove) throw new ConnectError('the tool changed since the request', Code.FailedPrecondition)
        if (req.argsSha256 !== current.argsSha256) throw new ConnectError('approval does not match the requested arguments', Code.Aborted)
        if (opts.changed && approves.length === 1) {
          // The request changed after the card loaded: the API refuses the old hash.
          current = create(ApprovalSchema, { ...current, argsSha256: changedSha })
          throw new ConnectError('approval does not match the requested arguments', Code.Aborted)
        }
        current = create(ApprovalSchema, { ...current, status: Status.EXECUTED, canDecide: false, decisionNote: req.note, result: { url: 'https://github.com/acme/widgets/pull/7' } })
        return { approval: current }
      },
      deny: (req) => {
        denies.push(req)
        current = create(ApprovalSchema, { ...current, status: Status.DENIED, canDecide: false, decisionNote: req.note })
        return { approval: current }
      },
    })
  })
  renderApp(transport, '/approvals/ap-1')
  return { approves, denies, gets: () => gets }
}

describe('riskItems', () => {
  it('orders push-and-run risk: CI, scripts, files made executable, dependencies, binaries, deletions, then size', () => {
    const items = riskItems(approval().risk)
    expect(items.map((i) => i.key)).toEqual(['ci', 'scripts', 'executable', 'dependencies', 'binary', 'deletions', 'size'])
    expect(items.map((i) => i.flagged)).toEqual([true, true, false, true, false, true, false])
    expect(riskItems(create(RiskSchema, { files: 120n, bytes: 10n })).at(-1)).toMatchObject({ key: 'size', flagged: true })
  })
})

describe('ApprovalCard', () => {
  it('leads with the risk summary, before the call details and the collapsed files', async () => {
    setup()
    const card = await screen.findByTestId('approval-card')
    const risk = within(card).getByRole('region', { name: 'Push and run risk' })
    const files = within(card).getByRole('region', { name: 'Files' })
    const branch = within(card).getByText('Branch')
    // Document order: risk, then branch/title, then files.
    expect(risk.compareDocumentPosition(branch) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(branch.compareDocumentPosition(files) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()

    const order = within(risk)
      .getAllByRole('listitem')
      .map((li) => li.dataset.risk)
      .filter(Boolean)
    expect(order).toEqual(['ci', 'scripts', 'executable', 'dependencies', 'binary', 'deletions', 'size'])
    expect(within(risk).getByText(/CI and automation config/)).toBeInTheDocument()

    // The per-file content is collapsed until opened.
    const details = within(files).getByText('README.md').closest('details')!
    expect(details.open).toBe(false)
    await userEvent.click(within(files).getByText('README.md'))
    expect(details.open).toBe(true)
    expect(within(details).getByLabelText('New content of README.md')).toHaveTextContent('receive')
  })

  it('shows a unified diff per file against the base branch, collapsed, with the reason where there is none', async () => {
    setup({
      diff: {
        base: 'main',
        files: [
          {
            path: 'README.md',
            status: FileStatus.MODIFIED,
            unifiedDiff: '--- a/README.md\n+++ b/README.md\n@@ -1,2 +1,2 @@\n # Widgets\n-recieve\n+receive\n',
          },
          { path: '.github/dependabot.yml', status: FileStatus.ADDED, unifiedDiff: '--- /dev/null\n+++ b/.github/dependabot.yml\n@@ -0,0 +1 @@\n+version: 2\n' },
          { path: 'scripts/release.sh', fallbackReason: 'GitHub answered 502 Bad Gateway when Koinon read the current version' },
          { path: 'old.txt', status: FileStatus.DELETED, unifiedDiff: '--- a/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-obsolete\n' },
        ],
      },
    })
    const card = await screen.findByTestId('approval-card')
    const files = within(card).getByRole('region', { name: 'Files' })
    expect(await within(files).findByText('Each file shows its diff against main as it is now.')).toBeInTheDocument()

    const readme = within(files).getByText('README.md').closest('details')!
    expect(readme.open).toBe(false)
    expect(within(readme).getByText('modified')).toBeInTheDocument()
    await userEvent.click(within(files).getByText('README.md'))
    expect(readme.open).toBe(true)
    const diff = within(readme).getByLabelText('Diff of README.md')
    expect([...diff.querySelectorAll('[data-line]')].map((l) => [l.getAttribute('data-line'), l.textContent])).toEqual([
      ['hunk', '@@ -1,2 +1,2 @@'],
      ['ctx', ' # Widgets'],
      ['del', '-recieve'],
      ['add', '+receive'],
    ])
    expect(within(readme).queryByLabelText('New content of README.md')).not.toBeInTheDocument()

    const added = within(files).getByText('.github/dependabot.yml').closest('details')!
    expect(within(added).getByText('added')).toBeInTheDocument()
    expect(within(added).getByLabelText('Diff of .github/dependabot.yml')).toHaveTextContent('+version: 2')

    const deleted = within(files).getByText('old.txt').closest('details')!
    expect(within(deleted).getByText('deleted')).toBeInTheDocument()
    expect(within(deleted).getByLabelText('Diff of old.txt')).toHaveTextContent('-obsolete')

    const fallback = within(files).getByText('scripts/release.sh').closest('details')!
    expect(within(fallback).getByRole('note')).toHaveTextContent('No diff: GitHub answered 502 Bad Gateway when Koinon read the current version. Showing the full new content.')
    expect(within(fallback).getByLabelText('New content of scripts/release.sh')).toHaveTextContent('echo hi')

    // The risk summary is unchanged by the diff.
    expect(within(card).getByRole('region', { name: 'Push and run risk' })).toHaveTextContent('CI and automation config')
  })

  it('flags a file made executable in the summary and on the file, with its mode change', async () => {
    setup({
      executable: true,
      diff: {
        base: 'main',
        files: [
          { path: 'README.md', status: FileStatus.MODIFIED, oldMode: '100644', newMode: '100644' },
          { path: '.github/dependabot.yml', status: FileStatus.ADDED, newMode: '100644' },
          {
            path: 'scripts/release.sh',
            status: FileStatus.MODIFIED,
            oldMode: '100644',
            newMode: '100755',
            unifiedDiff: '--- a/scripts/release.sh\n+++ b/scripts/release.sh\n@@ -1,2 +1,2 @@\n #!/bin/sh\n-echo old\n+echo hi\n',
          },
          { path: 'old.txt', status: FileStatus.DELETED },
        ],
      },
    })
    const card = await screen.findByTestId('approval-card')
    const risk = within(card).getByRole('region', { name: 'Push and run risk' })
    const item = risk.querySelector('[data-risk="executable"]') as HTMLElement
    expect(item.dataset.flagged).toBe('true')
    expect(item).toHaveTextContent('Makes executable')
    expect(item).toHaveTextContent('Written as mode 100755, so it can be run directly.')
    expect(item).toHaveTextContent('scripts/release.sh')

    const files = within(card).getByRole('region', { name: 'Files' })
    const script = within(files).getByText('scripts/release.sh').closest('details')!
    expect(within(script).getByText('makes executable')).toBeInTheDocument()
    expect(await within(script).findByTestId('mode-change')).toHaveTextContent('mode 100644 → 100755')
    const readme = within(files).getByText('README.md').closest('details')!
    expect(within(readme).queryByText('makes executable')).not.toBeInTheDocument()
    expect(within(readme).queryByTestId('mode-change')).not.toBeInTheDocument()
  })

  it('falls back to the full new content when the diff cannot be loaded', async () => {
    setup()
    const files = await screen.findByRole('region', { name: 'Files' })
    expect(await within(files).findByText(/^Could not compare with the base branch \(approvals are not configured on this server\)/)).toBeInTheDocument()
    await userEvent.click(within(files).getByText('README.md'))
    expect(within(files).getByLabelText('New content of README.md')).toHaveTextContent('receive')
  })

  it('shows a binary file without its content', async () => {
    setup({ diff: { base: 'main', files: [{ path: 'README.md', status: FileStatus.MODIFIED, binary: true }] } })
    const files = await screen.findByRole('region', { name: 'Files' })
    await within(files).findByText('Each file shows its diff against main as it is now.')
    await userEvent.click(within(files).getByText('README.md'))
    const readme = within(files).getByText('README.md').closest('details')!
    expect(within(readme).getByText('Binary file; its content is not shown.')).toBeInTheDocument()
    expect(within(readme).queryByLabelText('New content of README.md')).not.toBeInTheDocument()
    expect(within(readme).queryByLabelText('Diff of README.md')).not.toBeInTheDocument()
  })

  it('approves with the args_sha256 from GetApproval and shows the result', async () => {
    const { approves } = setup()
    await screen.findByTestId('approval-card')
    await userEvent.type(screen.getByLabelText('Note (optional)'), 'looks good')
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(approves).toHaveLength(1))
    expect(approves[0]).toMatchObject({ orgId, approvalId: 'ap-1', argsSha256: sha, note: 'looks good' })
    expect(await screen.findByRole('link', { name: 'https://github.com/acme/widgets/pull/7' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })

  it('asks to reload when the request changed since it was opened (Aborted, HTTP 409)', async () => {
    const { approves, gets } = setup({ changed: true })
    await screen.findByTestId('approval-card')
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('This request changed since you opened it. Reload to review the new version.')
    expect(alert).not.toHaveTextContent('does not match')
    const before = gets()

    await userEvent.click(within(alert).getByRole('button', { name: 'Reload' }))
    await waitFor(() => expect(gets()).toBeGreaterThan(before))
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())
    expect(screen.getByTitle(changedSha)).toBeInTheDocument()

    // Approving again binds to the hash of the version now shown.
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(approves).toHaveLength(2))
    expect(approves[0].argsSha256).toBe(sha)
    expect(approves[1].argsSha256).toBe(changedSha)
    expect(await screen.findByRole('link', { name: 'https://github.com/acme/widgets/pull/7' })).toBeInTheDocument()
  })

  it('renders an MCP call with its server and pretty-printed arguments, and approves the same hash', async () => {
    const { approves } = setup({ mcp: true })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByRole('heading', { name: 'Approve create_page on docs?' })).toBeInTheDocument()
    expect(within(card).getByText('Server')).toBeInTheDocument()
    expect(within(card).queryByText('Repository')).not.toBeInTheDocument()
    expect(within(card).queryByRole('region', { name: 'Push and run risk' })).not.toBeInTheDocument()
    const details = within(card).getByTestId('mcp-arguments') as HTMLDetailsElement
    expect(details.open).toBe(false)
    expect(within(card).getByLabelText('Call arguments').textContent).toBe(JSON.stringify({ title: 'Hello', tags: ['a', 'b'] }, null, 2))
    await userEvent.click(within(card).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(approves).toHaveLength(1))
    expect(approves[0].argsSha256).toBe(sha)
  })

  it('says which legs the leak guard saw on a card it wrote', async () => {
    setup({ mcp: true, mcpInit: { leakGuard: { untrustedInput: true, privateData: true } } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByTestId('leak-guard-legs')).toHaveTextContent('untrusted input and private data')
    expect(within(card).getByText(/url: https:\/\/attacker\.invalid\/x/)).toBeInTheDocument()
  })

  it('shows no leak guard note on an ordinary ask-tier card', async () => {
    setup({ mcp: true })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).queryByTestId('leak-guard')).not.toBeInTheDocument()
  })

  it('shows the tool version, its description collapsed, and a review notice for an MCP call', async () => {
    setup({ mcp: true, mcpInit: { reviewReason: 'the description changed', toolDescription: 'Creates a page.' } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByText('abcdef01')).toBeInTheDocument()
    expect(within(card).getByText(/Tool version/)).toBeInTheDocument()
    expect(within(card).getByText('the description changed')).toBeInTheDocument()
    expect((within(card).getByTestId('mcp-tool-description') as HTMLDetailsElement).open).toBe(false)
    expect(within(card).getByText('What the tool says it does')).toBeInTheDocument()
    expect(within(card).queryByText(/The tool changed after this request/)).not.toBeInTheDocument()
    expect(within(card).getByRole('button', { name: 'Approve' })).toBeEnabled()
  })

  it('warns and disables Approve when the tool changed after the request', async () => {
    setup({ mcp: true, mcpInit: { currentContentHash: '99999999aaaa' } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getAllByText(/The tool changed after this request; Koinon will not run it/).length).toBeGreaterThan(0)
    expect(within(card).getByText(/Deny it and let the agent ask again/)).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(within(card).getByRole('button', { name: 'Deny' })).toBeEnabled()
  })

  it('does not warn when the current hash equals the requested one', async () => {
    setup({ mcp: true, mcpInit: { currentContentHash: 'abcdef0123456789' } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).queryByText(/The tool changed after this request/)).not.toBeInTheDocument()
  })

  it('labels the first of two approvals', async () => {
    setup({ mcp: true, mcpInit: { approvalsRequired: 2 } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByText('Needs two approvers')).toBeInTheDocument()
    expect(within(card).queryByText(/First approval given/)).not.toBeInTheDocument()
    expect(within(card).getByRole('button', { name: 'Give first approval' })).toBeEnabled()
  })

  it('offers the second approval to someone other than the first approver', async () => {
    setup({ mcp: true, mcpInit: { approvalsRequired: 2, firstApprover: 'u-other' } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByText(/First approval given; a second person \(not the first approver, not the task's creator\) must approve\./)).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: 'Give second approval' })).toBeEnabled()
  })

  it('stops the first approver from giving the second approval', async () => {
    setup({ mcp: true, mcpInit: { approvalsRequired: 2, firstApprover: 'u-1' } })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByRole('button', { name: 'Give second approval' })).toBeDisabled()
    expect(within(card).getByText(/You gave the first approval; a different admin or owner must give the second/)).toBeInTheDocument()
  })

  it('shows a FailedPrecondition from Approve inline', async () => {
    setup({ mcp: true, mcpInit: { errorOnApprove: true } })
    const card = await screen.findByTestId('approval-card')
    await userEvent.click(within(card).getByRole('button', { name: 'Approve' }))
    expect(await within(card).findByText('the tool changed since the request')).toBeInTheDocument()
  })

  it('denies with a note', async () => {
    const { denies, approves } = setup()
    await screen.findByTestId('approval-card')
    await userEvent.type(screen.getByLabelText('Note (optional)'), 'wrong repo')
    await userEvent.click(screen.getByRole('button', { name: 'Deny' }))
    await waitFor(() => expect(denies).toHaveLength(1))
    expect(denies[0]).toMatchObject({ approvalId: 'ap-1', note: 'wrong repo' })
    expect(approves).toHaveLength(0)
  })

  it('is read-only for members', async () => {
    setup({ role: OrgRole.MEMBER, canDecide: false })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByRole('region', { name: 'Push and run risk' })).toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: 'Deny' })).not.toBeInTheDocument()
    expect(within(card).getByText(/Only org admins and owners can approve or deny/)).toBeInTheDocument()
  })

  it('stays read-only for a member even if the API said otherwise', async () => {
    setup({ role: OrgRole.MEMBER, canDecide: true })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })

  describe('scope choice', () => {
    it('approves once by default, without a scope', async () => {
      const { approves } = setup({ mcp: true })
      await screen.findByTestId('approval-card')
      expect(screen.getByLabelText('How far does this approval reach?')).toHaveValue('once')
      await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
      await waitFor(() => expect(approves).toHaveLength(1))
      expect(approves[0].scope).toBe(ScopeKind.UNSPECIFIED)
      expect(approves[0].scopeUntil).toBeUndefined()
    })

    it('approves for this agent until a date', async () => {
      const { approves } = setup({ mcp: true })
      await screen.findByTestId('approval-card')
      await userEvent.selectOptions(screen.getByLabelText('How far does this approval reach?'), 'agent')
      const until = screen.getByLabelText('Until (end of day)')
      await userEvent.clear(until)
      const want = new Date(Date.now() + 30 * 24 * 3600 * 1000)
      await userEvent.type(until, localDate(want))
      expect(screen.getByText(/always ask again/)).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
      await waitFor(() => expect(approves).toHaveLength(1))
      expect(approves[0].scope).toBe(ScopeKind.AGENT)
      const sent = timestampDate(approves[0].scopeUntil!)
      expect([sent.getFullYear(), sent.getMonth(), sent.getDate()]).toEqual([want.getFullYear(), want.getMonth(), want.getDate()])
    })

    it('approves for this task', async () => {
      const { approves } = setup({ mcp: true })
      await screen.findByTestId('approval-card')
      await userEvent.selectOptions(screen.getByLabelText('How far does this approval reach?'), 'task')
      expect(screen.queryByLabelText('Until (end of day)')).not.toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
      await waitFor(() => expect(approves).toHaveLength(1))
      expect(approves[0].scope).toBe(ScopeKind.TASK)
    })

    it('does not accept a date beyond the server limit, and offers a min and max', async () => {
      setup({ mcp: true })
      await screen.findByTestId('approval-card')
      await userEvent.selectOptions(screen.getByLabelText('How far does this approval reach?'), 'agent')
      const until = screen.getByLabelText('Until (end of day)')
      expect(until).toHaveAttribute('min')
      expect(until).toHaveAttribute('max')
      await userEvent.clear(until)
      await userEvent.type(until, localDate(new Date(Date.now() + 365 * 24 * 3600 * 1000)))
      expect(screen.getByText('Choose a date from tomorrow to a year ahead.')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled()
    })

    it('does not accept an agent scope without a future date', async () => {
      const { approves } = setup({ mcp: true })
      await screen.findByTestId('approval-card')
      await userEvent.selectOptions(screen.getByLabelText('How far does this approval reach?'), 'agent')
      const until = screen.getByLabelText('Until (end of day)')
      await userEvent.clear(until)
      await userEvent.type(until, '2001-01-01')
      expect(screen.getByText('Choose a date from tomorrow to a year ahead.')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled()
      expect(approves).toHaveLength(0)
    })

    it('offers no scope to a viewer or for a request that needs two approvers', async () => {
      setup({ role: OrgRole.MEMBER, canDecide: false })
      await screen.findByTestId('approval-card')
      expect(screen.queryByLabelText('How far does this approval reach?')).not.toBeInTheDocument()
    })

    it('offers no scope for two approvers', async () => {
      setup({ mcp: true, mcpInit: { approvalsRequired: 2 } })
      await screen.findByTestId('approval-card')
      expect(screen.queryByLabelText('How far does this approval reach?')).not.toBeInTheDocument()
    })
  })
})
