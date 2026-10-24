// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { ApprovalSchema, ApprovalService, Decision, SavedFileSchema, SavedWorkCallSchema, Status, type ApproveRequest, type DenyRequest } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const sha = 'c'.repeat(64)

function savedApproval(opts: { canDecide?: boolean; savedStatus?: string; status?: Status } = {}) {
  return create(ApprovalSchema, {
    id: 'ap-1',
    orgId,
    ticketId: 't-1',
    runId: 'r-1',
    connector: 'github',
    tool: 'push_saved_work',
    target: 'github:acme/widgets',
    argsSha256: sha,
    decision: Decision.ASK,
    status: opts.status ?? Status.PENDING,
    canDecide: opts.canDecide ?? true,
    system: true,
    call: {
      case: 'savedWork',
      value: create(SavedWorkCallSchema, {
        repository: 'acme/widgets',
        base: '0123456789abcdef0123456789abcdef01234567',
        branch: 'koinon/saved-work-t-1',
        files: [
          create(SavedFileSchema, { path: 'src/main.go', status: 'M', added: 12, deleted: 3, size: 400n }),
          create(SavedFileSchema, { path: 'docs/new.md', status: 'A', added: 5, deleted: 0, size: 90n }),
          create(SavedFileSchema, { path: 'old.txt', status: 'D', added: 0, deleted: 7, size: 0n }),
          create(SavedFileSchema, { path: 'logo.png', status: 'A', size: 2048n, binary: true }),
        ],
        sizeBytes: 2538n,
        skipped: ['node_modules/ (ignored directory)', 'big.bin (larger than 1 MiB)'],
        status: opts.savedStatus ?? 'saved',
        expiresAt: timestampFromMs(Date.UTC(2026, 10, 30, 12)),
      }),
    },
  })
}

function setup(opts: Parameters<typeof savedApproval>[0] = {}) {
  const approves: ApproveRequest[] = []
  const denies: DenyRequest[] = []
  let diffCalls = 0
  let current = savedApproval(opts)
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI() })
    service(ApprovalService, {
      getApproval: () => ({ approval: current }),
      getApprovalDiff: () => {
        diffCalls++
        throw new Error('saved work has no diff')
      },
      approve: (req) => {
        approves.push(req)
        current = create(ApprovalSchema, { ...current, status: Status.EXECUTED, canDecide: false, result: { url: 'https://github.com/acme/widgets/tree/koinon/saved-work-t-1' } })
        return { approval: current }
      },
      deny: (req) => {
        denies.push(req)
        current = create(ApprovalSchema, { ...current, status: Status.DENIED, canDecide: false })
        return { approval: current }
      },
    })
  })
  renderApp(transport, '/approvals/ap-1')
  return { approves, denies, diffCalls: () => diffCalls }
}

describe('ApprovalCard for saved work', () => {
  it('shows the diffstat: repository, branch, base, files with accessible status, totals, size, expiry and skipped notes', async () => {
    const { diffCalls } = setup()
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByRole('heading', { name: 'Push saved work to a branch' })).toBeInTheDocument()
    expect(within(card).getByText(/Koinon saved this uncommitted work from a sandbox that had to be recreated\. Nothing is pushed until you approve\. It goes to a new branch, never the default branch\./)).toBeInTheDocument()
    expect(within(card).getAllByText('acme/widgets').length).toBeGreaterThan(0)
    expect(within(card).getByText('koinon/saved-work-t-1')).toHaveClass('font-mono')
    expect(within(card).getByText('0123456')).toBeInTheDocument()
    expect(within(card).getByText('2.5 KiB')).toBeInTheDocument()
    expect(within(card).getByText('Kept until')).toBeInTheDocument()

    const table = within(card).getByRole('table', { name: 'Saved files' })
    const rows = within(table).getAllByRole('row')
    // header, four files, totals
    expect(rows).toHaveLength(6)
    expect(rows[1]).toHaveTextContent('src/main.go')
    expect(rows[1]).toHaveTextContent('M (Modified)')
    expect(rows[1]).toHaveTextContent('+12 -3')
    expect(rows[2]).toHaveTextContent('A (Added)')
    expect(rows[3]).toHaveTextContent('D (Deleted)')
    expect(rows[3]).toHaveTextContent('+0 -7')
    expect(rows[4]).toHaveTextContent('binary, 2.0 KiB')
    expect(rows[5]).toHaveTextContent('Total, 4 files')
    expect(rows[5]).toHaveTextContent('+17 -10')

    expect(within(card).getByText('node_modules/ (ignored directory)')).toBeInTheDocument()
    expect(within(card).getByText('big.bin (larger than 1 MiB)')).toBeInTheDocument()
    expect(within(card).queryByRole('region', { name: 'Push and run risk' })).not.toBeInTheDocument()
    expect(within(card).queryByRole('region', { name: 'Files' })).not.toBeInTheDocument()
    expect(diffCalls()).toBe(0)
  })

  it('approves with the args_sha256 from GetApproval, and denies with a note', async () => {
    const { approves } = setup()
    await screen.findByTestId('approval-card')
    await userEvent.type(screen.getByLabelText('Note (optional)'), 'go ahead')
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(approves).toHaveLength(1))
    expect(approves[0]).toMatchObject({ orgId, approvalId: 'ap-1', argsSha256: sha, note: 'go ahead' })
    expect(await screen.findByRole('link', { name: /koinon\/saved-work-t-1$/ })).toBeInTheDocument()
  })

  it('denies', async () => {
    const { denies } = setup()
    await screen.findByTestId('approval-card')
    await userEvent.click(screen.getByRole('button', { name: 'Deny' }))
    await waitFor(() => expect(denies).toHaveLength(1))
  })

  it('shows an expired one as expired, with no decision form', async () => {
    setup({ savedStatus: 'expired' })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByText(/This saved work has expired/)).toBeInTheDocument()
    expect(within(card).getByText('Expired', { selector: 'dt' })).toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })

  it('shows a pushed one as pushed, with no decision form', async () => {
    setup({ savedStatus: 'pushed', status: Status.EXECUTED, canDecide: false })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).getByText(/has been pushed to the branch/)).toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })

  it('leaves the buttons off for someone who cannot decide', async () => {
    setup({ canDecide: false })
    const card = await screen.findByTestId('approval-card')
    expect(within(card).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(within(card).getByText('Only org admins and owners can approve or deny this request.')).toBeInTheDocument()
  })
})
