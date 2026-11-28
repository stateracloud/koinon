// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { ErrorInfoSchema } from '@/gen/google/rpc/error_details_pb'
import { useFeedback } from '@/hooks/use-feedback'
import { resetToasts, toast } from '@/lib/toast'
import { Button } from './button'
import { InlineError, QueryError } from './inline-error'
import { Toaster } from './toast'

// The one feedback pattern (issue #1270): a toast for success, an inline error for failure, with the text of
// describeError (#777).

afterEach(() => act(() => resetToasts()))

function refusal(reason: string, message: string) {
  return new ConnectError(message, Code.PermissionDenied, undefined, [{ desc: ErrorInfoSchema, value: create(ErrorInfoSchema, { reason }) }])
}

function Action({ action, success }: { action: () => Promise<unknown>; success?: string }) {
  const f = useFeedback({ ADMIN_REQUIRED: 'Ask an admin to pause this agent.' })
  return (
    <>
      <Button onClick={() => void f.run(action, success)}>Pause</Button>
      <InlineError message={f.error} />
    </>
  )
}

describe('Toaster', () => {
  it('shows a toast as a polite status, dismisses it with its button, and keeps at most three', async () => {
    const user = userEvent.setup()
    render(<Toaster />)
    act(() => {
      toast('One.')
      toast('Two.')
      toast('Three.')
      toast('Four.')
    })
    // The viewport has its own name, so it does not clash with the page's Notifications region.
    expect(screen.getByRole('region', { name: 'Status messages (F8)' })).toBeInTheDocument()
    expect(screen.queryByText('One.')).toBeNull()
    for (const m of ['Two.', 'Three.', 'Four.']) expect(screen.getByText(m)).toBeInTheDocument()
    const status = screen.getByText('Four.').closest('[data-slot="toast"]')!
    expect(status).toHaveAttribute('data-state', 'open')
    // Radix announces a background toast politely: a status live copy with aria-live=polite, never an alert.
    await waitFor(() => {
      const live = screen.getAllByRole('status').find((s) => s.textContent === 'Status message Four.')
      expect(live).toHaveAttribute('aria-live', 'polite')
    })
    expect(screen.queryByRole('alert')).toBeNull()
    await user.click(screen.getAllByRole('button', { name: 'Dismiss' })[2])
    await waitFor(() => expect(screen.queryByText('Four.')).toBeNull())
  })

  it('closes a toast after its duration', async () => {
    render(<Toaster duration={50} />)
    act(() => {
      toast('Saved.')
    })
    expect(screen.getByText('Saved.')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText('Saved.')).toBeNull(), { timeout: 2000 })
  })
})

describe('useFeedback', () => {
  it('toasts on success and shows no error', async () => {
    const user = userEvent.setup()
    render(
      <>
        <Toaster />
        <Action action={() => Promise.resolve()} success="Agent paused." />
      </>,
    )
    await user.click(screen.getByRole('button', { name: 'Pause' }))
    expect(await screen.findByText('Agent paused.')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('shows a failure inline with the describeError text, the screen copy first, and no toast', async () => {
    const user = userEvent.setup()
    render(
      <>
        <Toaster />
        <Action action={() => Promise.reject(refusal('ADMIN_REQUIRED', 'role admin required'))} success="Agent paused." />
      </>,
    )
    await user.click(screen.getByRole('button', { name: 'Pause' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Ask an admin to pause this agent.')
    expect(screen.queryByText('Agent paused.')).toBeNull()
  })

  it('shows the server message for an error without a known reason, and clears it on the next try', async () => {
    const user = userEvent.setup()
    let fail = true
    render(<Action action={() => (fail ? Promise.reject(new ConnectError('agent is archived', Code.FailedPrecondition)) : Promise.resolve())} />)
    await user.click(screen.getByRole('button', { name: 'Pause' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('agent is archived')
    fail = false
    await user.click(screen.getByRole('button', { name: 'Pause' }))
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
  })
})

describe('InlineError and QueryError', () => {
  it('render nothing without an error', () => {
    const { container } = render(
      <>
        <InlineError />
        <QueryError error={null} />
      </>,
    )
    expect(container).toBeEmptyDOMElement()
  })

  it('QueryError words a failed read through describeError', () => {
    render(<QueryError error={refusal('PLATFORM_ADMIN_REQUIRED', 'raw text')} />)
    expect(screen.getByRole('alert')).toHaveTextContent('Only platform admins can do that.')
  })
})
