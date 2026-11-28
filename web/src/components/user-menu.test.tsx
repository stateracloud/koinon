// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { compile } from 'tailwindcss'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UserMenu } from '@/components/user-menu'
import { THEME_STORAGE_KEY } from '@/lib/theme'

function prefersLight(light: boolean) {
  vi.stubGlobal('matchMedia', (q: string) => ({
    matches: light && q.includes('light'),
    media: q,
    addEventListener: () => {},
    removeEventListener: () => {},
  }))
}

const theme = () => document.documentElement.getAttribute('data-theme')

beforeEach(() => {
  document.documentElement.removeAttribute('data-theme')
  prefersLight(false)
})
afterEach(() => vi.unstubAllGlobals())

function renderMenu(onSignOut = () => {}) {
  render(<UserMenu name="Admin" email="admin@example.test" onSignOut={onSignOut} signingOut={false} />)
}

async function pickTheme(label: string) {
  await userEvent.click(screen.getByRole('button', { name: 'User menu: Admin' }))
  await userEvent.click(await screen.findByRole('menuitemradio', { name: label }))
}

describe('UserMenu theme switch', () => {
  it('System with no preference is dark', () => {
    renderMenu()
    expect(theme()).toBe('dark')
  })

  it('System follows prefers-color-scheme: light', () => {
    prefersLight(true)
    renderMenu()
    expect(theme()).toBe('light')
  })

  it('remembers the choice and applies it', async () => {
    renderMenu()
    await pickTheme('Light')
    expect(theme()).toBe('light')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
  })

  it('starts from the saved choice', async () => {
    localStorage.setItem(THEME_STORAGE_KEY, 'light')
    renderMenu()
    expect(theme()).toBe('light')
    await userEvent.click(screen.getByRole('button', { name: 'User menu: Admin' }))
    expect(await screen.findByRole('menuitemradio', { name: 'Light' })).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('menuitemradio', { name: 'System' })).toHaveAttribute('aria-checked', 'false')
  })

  it('still works when storage throws', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    renderMenu()
    await pickTheme('Light')
    expect(theme()).toBe('light')
    vi.restoreAllMocks()
  })

  it('works with the keyboard alone and signs out', async () => {
    const onSignOut = vi.fn()
    renderMenu(onSignOut)
    await userEvent.tab()
    expect(screen.getByRole('button', { name: 'User menu: Admin' })).toHaveFocus()
    await userEvent.keyboard('{Enter}')
    const dark = await screen.findByRole('menuitemradio', { name: 'Dark' })
    while (document.activeElement !== dark) await userEvent.keyboard('{ArrowDown}')
    await userEvent.keyboard('{Enter}')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('dark')
    await userEvent.keyboard('{Enter}')
    const signOut = await screen.findByRole('menuitem', { name: 'Sign out' })
    while (document.activeElement !== signOut) await userEvent.keyboard('{ArrowDown}')
    await userEvent.keyboard('{Enter}')
    expect(onSignOut).toHaveBeenCalledOnce()
  })
})

describe('theme-init.js (runs before the first paint)', () => {
  const script = readFileSync(resolve(process.cwd(), 'public/theme-init.js'), 'utf8')
  const run = () => new Function(script)()

  it('uses the same storage key as the app', () => {
    expect(script).toContain(`'${THEME_STORAGE_KEY}'`)
  })
  it('sets dark with no saved choice and no preference', () => {
    run()
    expect(theme()).toBe('dark')
  })
  it('sets the saved choice', () => {
    localStorage.setItem(THEME_STORAGE_KEY, 'light')
    run()
    expect(theme()).toBe('light')
  })
  it('follows the system preference for System', () => {
    prefersLight(true)
    run()
    expect(theme()).toBe('light')
  })
})

describe('the dark: variant', () => {
  it('applies under data-theme="dark" and not under data-theme="light"', async () => {
    const css = readFileSync(resolve(process.cwd(), 'src/index.css'), 'utf8')
    const compiled = await compile(css, {
      loadStylesheet: async (id, base) => {
        const file = id === 'tailwindcss' ? 'node_modules/tailwindcss/index.css' : id === 'tw-animate-css' ? 'node_modules/tw-animate-css/dist/tw-animate.css' : null
        // The font packages add no utilities: an empty sheet stands in for them.
        const content = file ? readFileSync(resolve(process.cwd(), file), 'utf8') : ''
        return { path: id, base, content }
      },
    })
    const out = compiled.build(['dark:bg-raised'])
    expect(out).toContain('dark\\:bg-raised')
    expect(out).toContain('data-theme="light"')
    expect(out).not.toMatch(/\.dark\b[^\\-]/)

    const el = document.createElement('div')
    el.className = 'dark:bg-raised'
    document.body.append(el)
    // The rule's selector as the browser would match it, with the class selector escaped as in the output.
    const rule = /(\.dark\\:bg-raised:not\([^{]+\))\s*\{/.exec(out)?.[1] 
    expect(rule).toBeTruthy()
    document.documentElement.setAttribute('data-theme', 'dark')
    expect(el.matches(rule!)).toBe(true)
    document.documentElement.setAttribute('data-theme', 'light')
    expect(el.matches(rule!)).toBe(false)
    // Dark is the default: with no attribute (theme-init.js blocked) the variant applies too.
    document.documentElement.removeAttribute('data-theme')
    expect(el.matches(rule!)).toBe(true)
    el.remove()
  })
})
