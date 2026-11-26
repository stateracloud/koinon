// SPDX-License-Identifier: Apache-2.0

// Local oxlint plugin (JS plugin API). Rule no-raw-message: only src/lib/api-error.ts reads
// `rawMessage` of an API error. A screen shows describeError(e).message (ADR 0116 decision 5, #778).

const allowed = /(^|[\\/])src[\\/]lib[\\/]api-error\.ts$/

const noRawMessage = {
  meta: {
    type: 'problem',
    docs: { description: 'Forbid reading `rawMessage` outside web/src/lib/api-error.ts.' },
    schema: [],
    messages: { raw: 'Do not read `rawMessage`. Use describeError(e).message from @/lib/api-error.' },
  },
  create(context) {
    if (allowed.test(context.filename ?? context.getFilename())) return {}
    return {
      MemberExpression(node) {
        const p = node.property
        if (!node.computed && p && p.name === 'rawMessage') context.report({ node: p, messageId: 'raw' })
        if (node.computed && p && p.type === 'Literal' && p.value === 'rawMessage') context.report({ node: p, messageId: 'raw' })
      },
    }
  },
}

export default { meta: { name: 'koinon' }, rules: { 'no-raw-message': noRawMessage } }
