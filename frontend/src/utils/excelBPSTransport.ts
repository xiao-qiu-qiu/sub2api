export type ExcelBPSTransport = 'sse' | 'ws'

export function excelBPSTransport(value: unknown): ExcelBPSTransport {
  return value === 'ws' ? 'ws' : 'sse'
}
