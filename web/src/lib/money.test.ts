// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { amountError, formatPico, parsePico, percent, remaining } from '@/lib/money'

describe('parsePico and formatPico', () => {
  it.each([
    ['0', '0.00'],
    ['12.5', '12.50'],
    ['0.000000000001', '0.000000000001'],
    ['1000000', '1000000.00'],
    ['3.141592653589', '3.141592653589'],
  ])('%s round-trips as %s', (s, want) => {
    expect(formatPico(parsePico(s)!)).toBe(want)
  })

  it.each(['', '-1', '1e3', '1.1234567890123', '12345678', 'abc', '1,5'])('refuses %j', (s) => {
    expect(parsePico(s)).toBeUndefined()
  })
})

describe('remaining', () => {
  it('subtracts exactly and stops at zero', () => {
    expect(remaining('50.00', '12.30')).toBe('37.70')
    expect(remaining('0.1', '0.2')).toBe('0.00')
    expect(remaining('10', 'x')).toBeUndefined()
  })
})

describe('percent', () => {
  it('is the share of the whole with one decimal', () => {
    expect(percent('25', '200')).toBe(12.5)
    expect(percent('1', '3')).toBe(33.3)
    expect(percent('5', '0')).toBeUndefined()
    expect(percent('x', '1')).toBeUndefined()
  })
})

describe('amountError', () => {
  it('accepts what the server accepts', () => {
    expect(amountError('Cap', '50')).toBeUndefined()
    expect(amountError('Cap', '1000000')).toBeUndefined()
    expect(amountError('Cap', '0.5')).toBeUndefined()
  })
  it('refuses zero, too much, and text', () => {
    expect(amountError('Cap', '0')).toMatch(/more than 0/)
    expect(amountError('Cap', '1000000.01')).toMatch(/at most/)
    expect(amountError('Cap', 'ten')).toMatch(/dollar amount/)
    expect(amountError('Cap', '')).toMatch(/dollar amount/)
  })
})
