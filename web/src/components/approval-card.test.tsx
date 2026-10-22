// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import {
  ApprovalSchema,
  ApprovalService,
  Decision,
  FileChangeSchema,
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

function approval(opts: { canDecide?: boolean; status?: Status } = {}) {
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
          create(FileChangeSchema, { path: 'scripts/release.sh', content: '#!/bin/sh\necho hi\n' }),
          create(FileChangeSchema, { path: 'old.txt', delete: true }),
        ],
      }),
    },
  })
}

const changedSha = 'a'.repeat(64)

function setup(opts: { role?: OrgRole; canDecide?: boolean; changed?: boolean } = {}) {
  const approves: ApproveRequest[] = []
  const denies: DenyRequest[] = []
  let current = approval({ canDecide: opts.canDecide })
  let gets = 0
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ApprovalService, {
      getApproval: () => {
        gets++
        return { approval: current }
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
  it('orders push-and-run risk: CI, scripts, dependencies, binaries, deletions, then size', () => {
    const items = riskItems(approval().risk)
    expect(items.map((i) => i.key)).toEqual(['ci', 'scripts', 'dependencies', 'binary', 'deletions', 'size'])
    expect(items.map((i) => i.flagged)).toEqual([true, true, true, false, true, false])
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
    expect(order).toEqual(['ci', 'scripts', 'dependencies', 'binary', 'deletions', 'size'])
    expect(within(risk).getByText(/CI and automation config/)).toBeInTheDocument()

    // The per-file content is collapsed until opened.
    const details = within(files).getByText('README.md').closest('details')!
    expect(details.open).toBe(false)
    await userEvent.click(within(files).getByText('README.md'))
    expect(details.open).toBe(true)
    expect(within(details).getByLabelText('New content of README.md')).toHaveTextContent('receive')
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
