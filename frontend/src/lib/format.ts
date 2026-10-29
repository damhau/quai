const units: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["week", 7 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
]
const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" })

/** ago says how long ago a time was: "5 minutes ago", "yesterday". */
export function ago(at: string | Date, now = new Date()): string {
  const seconds = (new Date(at).getTime() - now.getTime()) / 1000
  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size) return rtf.format(Math.round(seconds / size), unit)
  }
  return "just now"
}

export const shortSha = (sha: string) => sha.slice(0, 7)

/** webURL is a run's page when it is one to open: http or https (a Flux run's is flux://…). */
export const webURL = (url?: string) => (url && /^https?:\/\//i.test(url) ? url : undefined)
