// SPDX-License-Identifier: Apache-2.0

// Group "agents": the new-agent flow and the agent page with the rollback dialog. router.tsx lazy-loads these exports.

import { Agent, AgentRollback } from '@/preview/agents/agent-page'
import { NewAgentBlank, NewAgentGallery, NewAgentTemplate } from '@/preview/agents/new-agent'

export function NewAgentGalleryPage() {
  return <NewAgentGallery />
}

export function NewAgentTemplatePage() {
  return <NewAgentTemplate />
}

export function NewAgentBlankPage() {
  return <NewAgentBlank />
}

export function AgentPage() {
  return <Agent />
}

export function AgentRollbackPage() {
  return <AgentRollback />
}
