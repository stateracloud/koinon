// SPDX-License-Identifier: Apache-2.0

import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { DataTable } from './data-table'

type Row = { id: string; name: string; size: number }

const col = dataTableColumns<Row>()
const columns: DataTableColumn<Row>[] = [
  col.accessor('name', { header: 'Name', meta: { rowHeader: true } }),
  col.accessor('size', { header: 'Size', sortFn: 'basic' }),
  col.display({ id: 'actions', header: () => <span className="sr-only">Actions</span>, cell: (c) => <button type="button">Open {c.row.original.name}</button>, enableSorting: false }),
]
const rows: Row[] = [
  { id: 'b', name: 'beta', size: 2 },
  { id: 'a', name: 'alpha', size: 3 },
  { id: 'c', name: 'gamma', size: 1 },
]

function names() {
  return screen.getAllByRole('rowheader').map((c) => c.textContent)
}

describe('DataTable', () => {
  it('renders a named table in a focusable scroll region, with row headers', () => {
    render(<DataTable caption="Things" columns={columns} data={rows} getRowId={(r) => r.id} />)
    const table = screen.getByRole('table', { name: 'Things' })
    expect(screen.getByRole('region', { name: 'Things table' })).toHaveAttribute('tabindex', '0')
    expect(within(table).getAllByRole('columnheader')).toHaveLength(3)
    expect(names()).toEqual(['beta', 'alpha', 'gamma'])
    expect(screen.getByRole('button', { name: 'Open alpha' })).toBeInTheDocument()
  })

  it('sorts by a column header button and sets aria-sort', async () => {
    const user = userEvent.setup()
    render(<DataTable caption="Things" columns={columns} data={rows} getRowId={(r) => r.id} />)
    const nameHeader = screen.getByRole('columnheader', { name: 'Name' })
    expect(nameHeader).toHaveAttribute('aria-sort', 'none')
    await user.click(within(nameHeader).getByRole('button'))
    expect(nameHeader).toHaveAttribute('aria-sort', 'ascending')
    expect(names()).toEqual(['alpha', 'beta', 'gamma'])
    await user.click(within(nameHeader).getByRole('button'))
    expect(nameHeader).toHaveAttribute('aria-sort', 'descending')
    expect(names()).toEqual(['gamma', 'beta', 'alpha'])
    // A column without sorting has no button and no aria-sort.
    expect(screen.getByRole('columnheader', { name: 'Actions' })).not.toHaveAttribute('aria-sort')
  })

  it('takes an initial sort', () => {
    render(<DataTable caption="Things" columns={columns} data={rows} getRowId={(r) => r.id} initialSorting={[{ id: 'size', desc: false }]} />)
    expect(names()).toEqual(['gamma', 'beta', 'alpha'])
    expect(screen.getByRole('columnheader', { name: 'Size' })).toHaveAttribute('aria-sort', 'ascending')
  })

  it('shows the empty state in place of an empty table', () => {
    render(<DataTable caption="Things" columns={columns} data={[]} getRowId={(r) => r.id} empty={<p>Nothing here.</p>} />)
    expect(screen.queryByRole('table')).toBeNull()
    expect(screen.getByText('Nothing here.')).toBeInTheDocument()
  })
})
