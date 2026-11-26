// SPDX-License-Identifier: Apache-2.0

type E = { rawMessage: string }
export const a = (e: E) => e.rawMessage
export const b = (e: E) => e['rawMessage']
export const c = (e?: E) => e?.rawMessage
export const d = (e: E) => e[`rawMessage`]
export const f = (e: E) => {
  const { rawMessage } = e
  return rawMessage
}
export const g = ({ rawMessage }: E) => rawMessage
