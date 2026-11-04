export const MIN_ACCOUNT_PRIORITY = -9999
export const MAX_ACCOUNT_PRIORITY = 9999

export function isAccountPriority(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) &&
    value >= MIN_ACCOUNT_PRIORITY && value <= MAX_ACCOUNT_PRIORITY
}

export function parseAccountPriority(value: string): number | null {
  const text = value.trim()
  if (!/^[+-]?\d+$/.test(text)) return null
  const priority = Number(text)
  return isAccountPriority(priority) ? priority : null
}
