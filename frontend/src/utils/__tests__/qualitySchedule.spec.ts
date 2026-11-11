import { describe, expect, it } from 'vitest'
import { qualityScheduleMinutes, qualitySchedulePresets } from '../qualitySchedule'

const intervals: [number, string][] = [
  [1, '* * * * *'], [2, '*/2 * * * *'], [5, '*/5 * * * *'], [10, '*/10 * * * *'],
  [15, '*/15 * * * *'], [30, '*/30 * * * *'], [60, '0 * * * *'], [120, '0 */2 * * *'],
  [360, '0 */6 * * *'], [720, '0 */12 * * *'], [1440, '0 0 * * *'],
]

describe('quality schedule presets', () => {
  it('offers ordered, unique intervals that stay uniform across hour and day boundaries', () => {
    expect(qualitySchedulePresets).toEqual(intervals.map(([minutes, cron]) => ({ minutes, cron })))
    expect(new Set(qualitySchedulePresets.map(preset => preset.cron)).size).toBe(intervals.length)
    for (const { minutes } of qualitySchedulePresets) {
      expect(minutes < 60 ? 60 % minutes : 1440 % minutes).toBe(0)
    }
  })

  it.each(intervals)('recognizes the %s-minute preset %s', (minutes, cron) => {
    expect(qualityScheduleMinutes(cron)).toBe(minutes)
    expect(qualityScheduleMinutes('  ' + cron.replace(/ /g, '  ') + '  ')).toBe(minutes)
  })

  it.each(['', '13 9 * * 1-5', '*/7 * * * *', '0 */5 * * *', 'invalid', '* * * * * *'])('leaves custom or invalid schedule %j unclassified', cron => {
    expect(qualityScheduleMinutes(cron)).toBeNull()
  })
})
