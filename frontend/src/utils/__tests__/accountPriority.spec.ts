import { describe, expect, it } from 'vitest'
import { isAccountPriority, parseAccountPriority } from '../accountPriority'

describe('account priority validation', () => {
  it.each([-9999, -1, 0, 1, 9999])('accepts the integer %s', priority => {
    expect(isAccountPriority(priority)).toBe(true)
    expect(parseAccountPriority(String(priority))).toBe(priority)
  })

  it('accepts surrounding whitespace and an explicit sign', () => {
    expect(parseAccountPriority(' -1 ')).toBe(-1)
    expect(parseAccountPriority('+0')).toBe(0)
  })

  it.each([-10000, 10000, 1.5, NaN, Infinity, null, undefined, '', '0', false])('rejects a non-integer or out-of-range value %s', value => {
    expect(isAccountPriority(value)).toBe(false)
  })

  it.each(['', ' ', '-', '+', '1.5', '1.0', '-1.5', '12abc', '1e2', '0x10', '1 2', '10000', '-10000'])('rejects incomplete or invalid integer text %j', value => {
    expect(parseAccountPriority(value)).toBeNull()
  })
})
