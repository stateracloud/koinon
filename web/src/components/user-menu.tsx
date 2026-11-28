// SPDX-License-Identifier: Apache-2.0

import { ChevronsUpDownIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { useThemeChoice } from '@/hooks/use-theme-choice'
import { isThemeChoice, THEME_CHOICES, type ThemeChoice } from '@/lib/theme'

const themeLabels: Record<ThemeChoice, string> = { system: 'System', dark: 'Dark', light: 'Light' }

/**
 * UserMenu is the user at the bottom of the sidebar: the name, the theme switch (System, Dark or Light; ADR 0126
 * section 1) and "Sign out". It stays mounted, so the theme effect always runs.
 */
export function UserMenu({ name, email, onSignOut, signingOut }: { name: string; email: string; onSignOut: () => void; signingOut: boolean }) {
  const [theme, setTheme] = useThemeChoice()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" className="w-full justify-between px-3">
          <span className="truncate">
            <span className="sr-only">User menu:</span>{' '}
            {name}
          </span>
          <ChevronsUpDownIcon aria-hidden className="text-muted-foreground" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-56">
        <DropdownMenuLabel className="grid font-normal">
          <span className="font-semibold">{name}</span>
          {email && email !== name && <span className="truncate text-xs">{email}</span>}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuLabel className="text-xs">Theme</DropdownMenuLabel>
        <DropdownMenuRadioGroup
          aria-label="Theme"
          value={theme}
          onValueChange={(v) => {
            if (isThemeChoice(v)) setTheme(v)
          }}
        >
          {THEME_CHOICES.map((c) => (
            <DropdownMenuRadioItem key={c} value={c}>
              {themeLabels[c]}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem disabled={signingOut} onSelect={onSignOut}>
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
