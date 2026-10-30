// SPDX-License-Identifier: Apache-2.0

import { createMemoryHistory } from '@tanstack/react-router'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import App from '@/App'
import { screens } from '@/preview/screens'

function renderAt(path: string) {
  return render(<App history={createMemoryHistory({ initialEntries: [path] })} />)
}


// Every preview page is lazy-loaded; under a loaded CI runner the first import can take over the default 1 s.
const lazyLoad = { timeout: 5000 }
describe('UI v2 preview', () => {
  it('lists every mockup screen on the index, with no session', async () => {
    renderAt('/preview')
    expect(await screen.findByRole('heading', { level: 1, name: 'UI v2 preview' }, lazyLoad)).toBeInTheDocument()
    for (const s of screens) expect(screen.getAllByText(s.title).length).toBeGreaterThan(0)
  })

  it('themes the root from ?theme= and switches with the theme select', async () => {
    const user = userEvent.setup()
    const { container } = renderAt('/preview?theme=light')
    await screen.findByRole('heading', { level: 1, name: 'UI v2 preview' }, lazyLoad)
    const root = container.querySelector('.kp')
    expect(root).toHaveAttribute('data-theme', 'light')
    await user.selectOptions(screen.getByLabelText('Theme'), 'high-contrast')
    expect(root).toHaveAttribute('data-theme', 'high-contrast')
  })

  it('opens the command palette on Ctrl-K with actions for the selected run first', async () => {
    const user = userEvent.setup()
    renderAt('/preview/runs')
    const row = (await screen.findByRole('link', { name: '9c41d07a' }, lazyLoad)).closest('tr')!
    await user.click(within(row).getByText('Upgrade cert-manager to 1.19'))
    await user.keyboard('{Control>}k{/Control}')
    const palette = await screen.findByRole('dialog')
    expect(within(palette).getByText('For the selected run')).toBeInTheDocument()
    const options = within(palette).getAllByRole('option')
    expect(options[0]).toHaveTextContent('Open the run')
    await user.keyboard('{Enter}')
    expect(await screen.findByRole('heading', { level: 1, name: 'Upgrade cert-manager to 1.19' }, lazyLoad)).toBeInTheDocument()
  })

  it('opens the shortcuts sheet on ?', async () => {
    const user = userEvent.setup()
    renderAt('/preview/overview')
    await screen.findByRole('heading', { level: 1, name: 'Overview' }, lazyLoad)
    await user.keyboard('?')
    expect(await screen.findByRole('dialog', { name: 'Keyboard shortcuts' })).toBeInTheDocument()
  })

  it('switches space from the sidebar switcher', async () => {
    const user = userEvent.setup()
    renderAt('/preview/overview')
    await screen.findByRole('heading', { level: 1, name: 'Overview' }, lazyLoad)
    await user.click(screen.getByRole('button', { name: /Switch space/ }))
    await user.click(await screen.findByRole('menuitem', { name: /Platform admins only/ }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Orgs' }, lazyLoad)).toBeInTheDocument()
  })
})
