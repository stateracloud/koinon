// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { ciDirsError, ciPathFor, ciPathsHit, cleanPath, parseCiDirs } from '@/lib/ci-paths'

describe('ci paths', () => {
  it('cleans like the guard', () => {
    expect(cleanPath('./a//b/../c/')).toBe('a/c')
    expect(cleanPath('../x')).toBe('x')
    expect(cleanPath('a\\b')).toBe('a/b')
  })

  it('matches a path, a directory and the directory itself', () => {
    const ci = ['.github/workflows/', '.drone.yml']
    expect(ciPathFor('.github/workflows/ci.yml', ci, false)).toBe('.github/workflows/')
    expect(ciPathFor('.github/workflows', ci, false)).toBe('.github/workflows/')
    expect(ciPathFor('.drone.yml', ci, false)).toBe('.drone.yml')
    expect(ciPathFor('.github/workflows-docs/a.md', ci, false)).toBeUndefined()
    expect(ciPathFor('.drone.yml.bak', ci, false)).toBeUndefined()
  })

  it('lists each guarded entry once, in order', () => {
    expect(ciPathsHit(['b/x', 'a/y', 'a/z'], ['a', 'b'], false)).toEqual(['a', 'b'])
  })

  it('parses and checks the form field', () => {
    expect(parseCiDirs('a\n b ,c\r\n\n')).toEqual(['a', 'b', 'c'])
    expect(ciDirsError('a\nb')).toBeUndefined()
    expect(ciDirsError('/abs')).toMatch(/absolute/)
    expect(ciDirsError('a/../../b')).toMatch(/leaves/)
    expect(ciDirsError('.')).toMatch(/root/)
  })
})
