// SPDX-License-Identifier: Apache-2.0

import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Button } from './button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from './dialog'
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from './dropdown-menu'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from './sheet'
import { Tooltip, TooltipContent, TooltipTrigger } from './tooltip'

// The overlay primitives (issue #1270): each one is named, traps or returns focus as its ARIA pattern says, and
// closes with Escape.

describe('Dialog', () => {
  it('opens a named modal dialog, closes with Escape and returns focus to the trigger', async () => {
    const user = userEvent.setup()
    render(
      <Dialog>
        <DialogTrigger asChild>
          <Button>Rename agent</Button>
        </DialogTrigger>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rename agent</DialogTitle>
            <DialogDescription>The new name shows on every page.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button>Save</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>,
    )
    const trigger = screen.getByRole('button', { name: 'Rename agent' })
    await user.click(trigger)
    const dialog = screen.getByRole('dialog', { name: 'Rename agent' })
    expect(dialog).toHaveAccessibleDescription('The new name shows on every page.')
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(trigger).toHaveFocus()
  })

  it('can hide the close button', async () => {
    render(
      <Dialog open>
        <DialogContent showCloseButton={false}>
          <DialogTitle>Plain</DialogTitle>
          <DialogDescription>No close button.</DialogDescription>
        </DialogContent>
      </Dialog>,
    )
    expect(screen.getByRole('dialog', { name: 'Plain' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Close' })).toBeNull()
  })
})

describe('Sheet', () => {
  it('opens on the side it is given and closes with its close button', async () => {
    const user = userEvent.setup()
    render(
      <Sheet>
        <SheetTrigger asChild>
          <Button>Filters</Button>
        </SheetTrigger>
        <SheetContent side="left">
          <SheetHeader>
            <SheetTitle>Filters</SheetTitle>
            <SheetDescription>Narrow the list.</SheetDescription>
          </SheetHeader>
        </SheetContent>
      </Sheet>,
    )
    await user.click(screen.getByRole('button', { name: 'Filters' }))
    const sheet = screen.getByRole('dialog', { name: 'Filters' })
    expect(sheet).toHaveAttribute('data-side', 'left')
    await user.click(screen.getByRole('button', { name: 'Close' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })
})

describe('DropdownMenu', () => {
  it('opens a menu of items by keyboard and runs the chosen item', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const onCheckedChange = vi.fn()
    render(
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button>Actions</Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuLabel>Agent</DropdownMenuLabel>
          <DropdownMenuItem onSelect={onSelect}>Pause agent</DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuCheckboxItem checked={false} onCheckedChange={onCheckedChange}>
            Keep warm
          </DropdownMenuCheckboxItem>
        </DropdownMenuContent>
      </DropdownMenu>,
    )
    screen.getByRole('button', { name: 'Actions' }).focus()
    await user.keyboard('{Enter}')
    const menu = await screen.findByRole('menu')
    expect(menu).toBeInTheDocument()
    expect(screen.getByRole('menuitemcheckbox', { name: 'Keep warm' })).toHaveAttribute('aria-checked', 'false')
    await user.click(screen.getByRole('menuitem', { name: 'Pause agent' }))
    expect(onSelect).toHaveBeenCalledOnce()
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull())
  })
})

describe('Tooltip', () => {
  it('shows its hint on keyboard focus and hides it with Escape', async () => {
    const user = userEvent.setup()
    render(
      <Tooltip>
        <TooltipTrigger asChild>
          <Button aria-label="Copy run ID">#</Button>
        </TooltipTrigger>
        <TooltipContent>Copies the run ID</TooltipContent>
      </Tooltip>,
    )
    await user.tab()
    expect(screen.getByRole('button', { name: 'Copy run ID' })).toHaveFocus()
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Copies the run ID')
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('tooltip')).toBeNull())
  })
})
