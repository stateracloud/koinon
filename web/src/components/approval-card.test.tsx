// SPDX-License-Identifier: Apache-2.0

import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
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
  PullRequestCallSchema,
  RiskSchema,
  Status,
  type ApproveRequest,
  type DenyRequest,
} from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { riskItems } from '@/lib/risk'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const sha = 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'

function mcpApproval() {
  return create(ApprovalSchema, {
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

function setup(opts: { role?: OrgRole; canDecide?: boolean; changed?: boolean; diff?: DiffInit; executable?: boolean; mcp?: boolean } = {}) {
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
})
