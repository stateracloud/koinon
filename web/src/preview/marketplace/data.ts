// SPDX-License-Identifier: Apache-2.0

// Screen-only sample data of the marketplace group, seeded verbatim from Skill-add.dc.html and Skill-detail.dc.html.

export type ImportCandidate = { id: string; description: string; path: string; licence: string; files: number; size: string; needs?: string; picked: boolean }

export const importCandidates: ImportCandidate[] = [
  { id: 'pdf', description: 'Read, fill and merge PDF files', path: 'skills/pdf', licence: 'Proprietary, see LICENSE', files: 14, size: '212 KB', picked: true },
  { id: 'xlsx', description: 'Spreadsheets with formulas and charts', path: 'skills/xlsx', licence: 'Proprietary, see LICENSE', files: 9, size: '96 KB', needs: 'openpyxl', picked: true },
  { id: 'docx', description: 'Word documents', path: 'skills/docx', licence: 'Proprietary, see LICENSE', files: 21, size: '340 KB', picked: false },
  { id: 'mcp-builder', description: 'Build MCP servers', path: 'skills/mcp-builder', licence: 'Apache-2.0', files: 6, size: '48 KB', picked: false },
]

export const editorFiles: { name: string; pad: number; meta: string; error?: boolean }[] = [
  { name: 'SKILL.md', pad: 8, meta: '3.1 KB', error: true },
  { name: 'references/', pad: 8, meta: '' },
  { name: 'policy.md', pad: 24, meta: '4.0 KB' },
  { name: 'scripts/', pad: 8, meta: '' },
  { name: 'find_orphans.py', pad: 24, meta: '6.2 KB · x' },
  { name: 'stale_namespaces.py', pad: 24, meta: '3.9 KB · x' },
  { name: 'assets/', pad: 8, meta: '' },
  { name: 'report-template.md', pad: 24, meta: '0.8 KB' },
]

export const skillMdLines = ['---', 'name: k8s-hygiene', 'description: Find orphaned PVCs, empty namespaces and old failed Jobs, and propose clean-ups as one plan.', 'license: Apache-2.0', 'compatibility: python3 (standard library only)', '---', '', '# Kubernetes hygiene', '', 'Run scripts/find_orphans.py and scripts/stale_namespaces.py with the', 'read-only kubeconfig. Never act on kube-system or keep=true namespaces.', '', 'Follow the deletion rules in [the policy](references/policy.md) and the', 'size limits in [limits](references/limits.md).', '', 'Write the report with assets/report-template.md.']

/** The line of SKILL.md whose link does not resolve (1-based is index + 1). */
export const skillMdErrorLine = 13

export const pasteSample = `---
name: on-call-handoff
description: Write the on-call handoff note from the week's incidents and open alerts.
---

# On-call handoff
Read the incidents channel export and Alertmanager's open alerts, then write…`

export const scanFindings = [
  { severity: 'Medium', tone: 'outline', scanner: 'skill-scanner', rule: 'SUBPROCESS_SHELL', where: 'scripts/diff.py:41', message: 'subprocess.run with shell=True' },
  { severity: 'Low', tone: 'secondary', scanner: 'SkillSpector', rule: 'SS-DOC-004', where: 'SKILL.md:3', message: 'Description over 200 characters' },
] as const

export const skillFiles = [
  { name: 'SKILL.md', pad: 8, meta: '2.4 KB' },
  { name: 'scripts/diff.py', pad: 24, meta: '5.1 KB · executable' },
  { name: 'scripts/crd_changes.py', pad: 24, meta: '3.3 KB · executable' },
  { name: 'references/crd-upgrades.md', pad: 24, meta: '7.8 KB' },
]

export const skillVersions = [
  { label: 'v5 · 8e11…02bd', status: null, note: 'git 9b0e2d4 · imported by the daily check · today 06:00' },
  { label: 'v4 · 3c7a…41f0', status: 'Approved · gate:auto', note: 'git 51c8a77 · Andre Staats · 14 Oct' },
  { label: 'v3 · a402…9c11', status: 'Blocked', note: '"Wrote to the home volume" · Maria Okafor · 2 Oct' },
]

export const skillUsage = [
  { name: 'LCM Upgrader', note: 'v4 in v7 · v5 staged in v8', href: '/preview/agents/lcm-upgrader' },
  { name: 'Release Notes Scout', note: 'v4 in v3' },
  { name: 'Template LCM Upgrader', note: 'v4 · platform', plain: true },
] as const

/** Which category each sample item belongs to; the mockup only shows the counts, so this is a preview guess. */
export const itemCategory: Record<string, string> = {
  'Argo CD': 'Kubernetes',
  GitHub: 'Code hosts',
  Prometheus: 'Observability',
  Kubernetes: 'Kubernetes',
  'helm-upgrade': 'Kubernetes',
  pdf: 'Files and data',
}
