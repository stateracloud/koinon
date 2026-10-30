// SPDX-License-Identifier: Apache-2.0

// The preview's shared building blocks, in one import: `import { PreviewShell, StatusBadge, Panel } from '@/preview/ui'`.
// Classes: ks-* (layout, from the mockups' mock.css) and kn-* (design-system components) are global and resolve
// their colours from the tokens on the .kp root; Tailwind utilities work too and win over both.

export { Badge, Button, ButtonLink, type BadgeVariant, type ButtonProps } from '@/preview/components/button'
export { Alert, AlertDescription, AlertTitle, Composer, EmptyState, NoticeBanner, type AlertVariant, type ComposerProps } from '@/preview/components/feedback'
export { Field, Input, Label, NativeSelect, Textarea, TypeToConfirm } from '@/preview/components/field'
export { AgentAvatar, Logo, SpaceMark } from '@/preview/components/identity'
export { CodeBlock, FilterBar, Kbd, Keys, Meter, PageHeader, Panel, Pill, Section, Segmented, Table, TabPanel, Tabs, type FilterChip, type TabItem } from '@/preview/components/layout'
export { Dialog, DialogClose, Drawer, type DialogProps } from '@/preview/components/overlay'
export { AgeChip, RunStatus, StateDot, StatusBadge, type StatusBadgeProps } from '@/preview/components/status'
export { ageBand, ageText, buttonClass, describedBy, hueOf, initialsOf, markStyle, meterLevel, statusLabel, statusTone, type AgeBand, type ButtonSize, type ButtonVariant, type StatusKind } from '@/preview/lib/format'
export { ScreenStub } from '@/preview/components/stub'
export { usePaletteSelection, usePortal, usePreview, type PaletteAction, type PaletteSelection, type Theme } from '@/preview/lib/context'
export { PreviewLink, type PreviewLinkProps } from '@/preview/lib/link'
export { usePathname, usePreviewNavigate } from '@/preview/lib/navigation'
export { PreviewShell, ThemeSwitch, type Crumb, type PreviewShellProps } from '@/preview/shell/shell'
export type { Space } from '@/preview/shell/nav'
