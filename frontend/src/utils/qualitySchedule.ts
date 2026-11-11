export const qualitySchedulePresets = [1, 2, 5, 10, 15, 30, 60, 120, 360, 720, 1440].map(minutes => ({
  minutes,
  cron: minutes === 1 ? '* * * * *' : minutes < 60 ? `*/${minutes} * * * *`
    : minutes === 60 ? '0 * * * *' : minutes === 1440 ? '0 0 * * *' : `0 */${minutes / 60} * * *`,
}))
export function qualityScheduleMinutes(cron: string): number | null {
  return qualitySchedulePresets.find(preset => preset.cron === cron.trim().replace(/\s+/g, ' '))?.minutes ?? null
}
