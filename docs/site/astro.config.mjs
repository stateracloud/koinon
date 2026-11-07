// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from 'astro/config'
import starlight from '@astrojs/starlight'

// No analytics, no external fonts, no deploy from this repository's CI (plan item DOC1).
export default defineConfig({
  integrations: [
    starlight({
      title: 'Koinon',
      description: 'Install Koinon and run your first agent.',
      sidebar: [
        { label: 'Start here', items: ['prerequisites', 'quickstart'] },
        {
          label: 'Guides',
          items: ['guides/models', 'guides/prices', 'guides/budgets', 'guides/mcp-servers', 'guides/github-app', 'guides/ghes', 'guides/gitea', 'guides/oidc', 'guides/gateway-modes', 'guides/agent-health', 'guides/audit', 'guides/retention', 'guides/metrics', 'guides/private-registries', 'guides/upgrading', 'guides/backup-restore'],
        },
        {
          label: 'Working with agents',
          items: ['guides/tasks-and-inbox', 'guides/ask-user', 'guides/triggers', 'guides/scoped-approvals', 'guides/home-checkpoints'],
        },
      ],
    }),
  ],
})
