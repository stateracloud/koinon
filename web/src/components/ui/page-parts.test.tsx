// SPDX-License-Identifier: Apache-2.0

import { render, screen, within } from '@testing-library/react'
import { BotIcon } from 'lucide-react'
import { describe, expect, it } from 'vitest'
import { meterTone } from '@/lib/meter'
import { Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from './breadcrumb'
import { Button } from './button'
import { EmptyState } from './empty-state'
import { Meter } from './meter'
import { PageHeader } from './page-header'
import { Skeleton, SkeletonRows } from './skeleton'
import { StatTile } from './stat-tile'

describe('PageHeader', () => {
  it('has the one h1, the description, the breadcrumb and the actions', () => {
    render(
      <PageHeader
        title="Agents"
        description="The agents of this org."
        breadcrumb={<nav aria-label="Breadcrumb">crumbs</nav>}
        actions={<Button>New agent</Button>}
      />,
    )
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
    expect(screen.getByRole('heading', { level: 1, name: 'Agents' })).toBeInTheDocument()
    expect(screen.getByText('The agents of this org.')).toBeInTheDocument()
    expect(screen.getByRole('navigation', { name: 'Breadcrumb' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New agent' })).toBeInTheDocument()
  })

  it('needs only a title', () => {
    const { container } = render(<PageHeader title="API tokens" />)
    expect(screen.getByRole('heading', { level: 1, name: 'API tokens' })).toBeInTheDocument()
    expect(container.querySelector('header')?.children).toHaveLength(1)
  })
})

describe('Breadcrumb', () => {
  it('is a named navigation list; the last item is the current page and separators are hidden', () => {
    render(
      <Breadcrumb>
        <BreadcrumbList>
          <BreadcrumbItem>
            <BreadcrumbLink href="/agents">Agents</BreadcrumbLink>
          </BreadcrumbItem>
          <BreadcrumbSeparator />
          <BreadcrumbItem>
            <BreadcrumbPage>Repo helper</BreadcrumbPage>
          </BreadcrumbItem>
        </BreadcrumbList>
      </Breadcrumb>,
    )
    const nav = screen.getByRole('navigation', { name: 'Breadcrumb' })
    expect(within(nav).getAllByRole('listitem')).toHaveLength(2)
    expect(within(nav).getByRole('link', { name: 'Agents' })).toHaveAttribute('href', '/agents')
    expect(within(nav).getByText('Repo helper')).toHaveAttribute('aria-current', 'page')
  })

  it('renders a router link through asChild', () => {
    render(
      <BreadcrumbLink asChild>
        <a href="/x" data-router>
          X
        </a>
      </BreadcrumbLink>,
    )
    expect(screen.getByRole('link', { name: 'X' })).toHaveAttribute('data-slot', 'breadcrumb-link')
  })
})

describe('EmptyState', () => {
  it('says what is empty and offers the next step', () => {
    const { container } = render(<EmptyState icon={BotIcon} title="No agents yet." description="Create an agent to give it work." action={<Button>New agent</Button>} />)
    expect(screen.getByText('No agents yet.')).toBeInTheDocument()
    expect(screen.getByText('Create an agent to give it work.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New agent' })).toBeInTheDocument()
    expect(container.querySelector('svg')).toHaveAttribute('aria-hidden', 'true')
  })
})

describe('StatTile', () => {
  it('pairs the label with the value in a description list', () => {
    render(<StatTile label="Spend today" value="$12.40" hint="of $50.00 daily limit" />)
    const term = screen.getByRole('term')
    expect(term).toHaveTextContent('Spend today')
    const defs = screen.getAllByRole('definition')
    expect(defs.map((d) => d.textContent)).toEqual(['$12.40', 'of $50.00 daily limit'])
  })
})

describe('Meter', () => {
  it('is a named meter with its value in words', () => {
    render(<Meter label="Spend today" value={12.4} max={50} valueText="$12.40 of $50.00" />)
    const meter = screen.getByRole('meter', { name: 'Spend today' })
    expect(meter).toHaveAttribute('aria-valuenow', '12.4')
    expect(meter).toHaveAttribute('aria-valuemax', '50')
    expect(meter).toHaveAttribute('aria-valuetext', '$12.40 of $50.00')
    expect(meter).toHaveAttribute('data-tone', 'normal')
    expect(screen.getByText('$12.40 of $50.00')).toBeInTheDocument()
  })

  it('caps the bar at the limit and turns to the over tone', () => {
    render(<Meter label="Runs" value={12} max={10} valueText="12 of 10 runs" compact />)
    const meter = screen.getByRole('meter', { name: 'Runs' })
    expect(meter).toHaveAttribute('aria-valuenow', '10')
    expect(meter).toHaveAttribute('data-tone', 'over')
    expect((meter.firstElementChild as HTMLElement).style.width).toBe('100%')
    // Compact hides the visible text; the accessible value text stays.
    expect(screen.queryByText('12 of 10 runs')).toBeNull()
  })

  it.each([
    [0, 10, 'normal'],
    [7.9, 10, 'normal'],
    [8, 10, 'warning'],
    [10, 10, 'over'],
    [0, 0, 'normal'],
    [1, 0, 'over'],
  ] as const)('meterTone(%s, %s) is %s', (value, max, tone) => {
    expect(meterTone(value, max)).toBe(tone)
  })
})

describe('Skeleton', () => {
  it('is hidden decoration; SkeletonRows says the loading state in text', () => {
    const { container } = render(
      <>
        <Skeleton className="h-4" />
        <SkeletonRows label="Loading agents…" rows={4} />
      </>,
    )
    expect(container.querySelector('[data-slot="skeleton"]')).toHaveAttribute('aria-hidden', 'true')
    expect(screen.getByRole('status')).toHaveTextContent('Loading agents…')
    expect(container.querySelectorAll('[data-slot="skeleton-rows"] [data-slot="skeleton"]')).toHaveLength(4)
  })
})
