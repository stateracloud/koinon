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
    const report = (node) => context.report({ node, messageId: 'raw' })
    const isName = (n, computed) =>
      !!n &&
      ((!computed && n.type === 'Identifier' && n.name === 'rawMessage') ||
        (n.type === 'Literal' && n.value === 'rawMessage') ||
        (n.type === 'TemplateLiteral' && n.expressions.length === 0 && n.quasis[0]?.value.cooked === 'rawMessage'))
    return {
      // e.rawMessage, e?.rawMessage, e['rawMessage'], e[`rawMessage`]
      MemberExpression(node) {
        if (isName(node.property, node.computed)) report(node.property)
      },
      // const { rawMessage } = e, ({ rawMessage }) => ...
      Property(node) {
        if (node.parent?.type === 'ObjectPattern' && isName(node.key, node.computed)) report(node.key)
      },
    }
  },
}

export default { meta: { name: 'koinon' }, rules: { 'no-raw-message': noRawMessage } }
