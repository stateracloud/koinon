// SPDX-License-Identifier: Apache-2.0

import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Checkbox } from './checkbox'
import { Label } from './label'
import { Switch } from './switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from './tabs'

describe('Tabs', () => {
  it('shows one panel, moves with the arrow keys and selects on focus', async () => {
    const user = userEvent.setup()
    render(
      <Tabs defaultValue="agents">
        <TabsList aria-label="Views">
          <TabsTrigger value="agents">Agents</TabsTrigger>
          <TabsTrigger value="chart">Chart</TabsTrigger>
        </TabsList>
        <TabsContent value="agents">Agent cards</TabsContent>
        <TabsContent value="chart">Reports-to chart</TabsContent>
      </Tabs>,
    )
    expect(screen.getByRole('tablist', { name: 'Views' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Agents' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel')).toHaveTextContent('Agent cards')
    await user.click(screen.getByRole('tab', { name: 'Agents' }))
    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('tab', { name: 'Chart' })).toHaveFocus()
    expect(screen.getByRole('tab', { name: 'Chart' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel')).toHaveTextContent('Reports-to chart')
  })
})

describe('Switch', () => {
  it('is a labelled switch that toggles with Space', async () => {
    const user = userEvent.setup()
    const onCheckedChange = vi.fn()
    render(
      <>
        <Switch id="warm" onCheckedChange={onCheckedChange} />
        <Label htmlFor="warm">Keep warm</Label>
      </>,
    )
    const sw = screen.getByRole('switch', { name: 'Keep warm' })
    expect(sw).toHaveAttribute('aria-checked', 'false')
    sw.focus()
    await user.keyboard(' ')
    expect(sw).toHaveAttribute('aria-checked', 'true')
    expect(onCheckedChange).toHaveBeenCalledWith(true)
  })

  it('cannot change when disabled', async () => {
    const user = userEvent.setup()
    render(<Switch aria-label="Locked" disabled />)
    await user.click(screen.getByRole('switch', { name: 'Locked' }))
    expect(screen.getByRole('switch', { name: 'Locked' })).toHaveAttribute('aria-checked', 'false')
  })
})

describe('Checkbox', () => {
  it('is a labelled checkbox that toggles on a click of its label', async () => {
    const user = userEvent.setup()
    render(
      <>
        <Checkbox id="notify" />
        <Label htmlFor="notify">Notify me</Label>
      </>,
    )
    const box = screen.getByRole('checkbox', { name: 'Notify me' })
    expect(box).not.toBeChecked()
    await user.click(screen.getByText('Notify me'))
    expect(box).toBeChecked()
  })

  it('shows the mixed state', () => {
    render(<Checkbox aria-label="All rows" checked="indeterminate" />)
    expect(screen.getByRole('checkbox', { name: 'All rows' })).toHaveAttribute('aria-checked', 'mixed')
  })
})
