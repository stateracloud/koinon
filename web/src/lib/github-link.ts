// SPDX-License-Identifier: Apache-2.0

/** The results the GitHub link callback sends back in ?github_link= (internal/githublink). */
export type GitHubLinkResult = 'linked' | 'signin' | 'expired' | 'denied' | 'taken' | 'failed'

export const githubLinkResultText: Record<GitHubLinkResult, string> = {
  linked: 'Your GitHub login is linked. Koinon asks you to review the pull requests that agents open for you.',
  signin: 'You were not signed in to Koinon. Sign in, then link your GitHub login again.',
  expired: 'The link request expired or was already used. Start it again from the GitHub connection.',
  denied: 'GitHub did not link your login because you did not authorize the App.',
  taken: 'This GitHub account is already linked to another Koinon user in this org. An admin can remove that link.',
  failed: 'Koinon could not link your GitHub login. Try again later.',
}

/** parseGitHubLinkResult accepts only the known results of the callback. */
export function parseGitHubLinkResult(v: unknown): GitHubLinkResult | undefined {
  return typeof v === 'string' && Object.hasOwn(githubLinkResultText, v) ? (v as GitHubLinkResult) : undefined
}
