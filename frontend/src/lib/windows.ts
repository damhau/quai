import type { ChangeWindowView } from "@/api/generated/types.gen"

// How change windows read in the UI (docs/design/05-delivery-and-approvals.md#change-windows-and-freezes):
// their times in the browser's time zone, and as the window is written, in its own.

/** localTime is an instant in the browser's time zone. */
export function localTime(iso: string): string {
  return new Date(iso).toLocaleString([], { dateStyle: "medium", timeStyle: "short" })
}

/** zonedTime is an instant in an IANA time zone, named: "Tue, Jan 5, 2027, 00:00 Europe/Zurich". */
export function zonedTime(iso: string, zone: string): string {
  try {
    const at = new Date(iso).toLocaleString([], {
      timeZone: zone,
      weekday: "short",
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    })
    return `${at} ${zone}`
  } catch {
    return `${new Date(iso).toISOString().slice(0, 16).replace("T", " ")} UTC` // a zone this browser does not know
  }
}

/** browserZone is the browser's IANA time zone. */
export const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone

/** bothTimes is an instant in the browser's time zone, then in the window's when that is another. */
export function bothTimes(iso: string, zone?: string): string {
  const local = localTime(iso)
  if (!zone || zone === browserZone()) return local
  return `${local} (${zonedTime(iso, zone)})`
}

const names = (ws?: string[] | null) => (ws ?? []).join(", ")

/**
 * windowText says what its change windows mean for a request not merged yet: when the next merge
 * can be, while they are closed; until when it can merge, while they are open.
 */
export function windowText(state: string, w: ChangeWindowView): string {
  const pending = state === "pending_approval"
  if (w.error)
    return pending
      ? `Its change windows cannot be decided (${w.error}): once approved, it waits until this is fixed.`
      : `Approved, but held: its change windows cannot be decided (${w.error}). It does not merge until this is fixed.`
  if (w.open) {
    const until = w.closes ? ` until ${bothTimes(w.closes, w.zone)}` : ""
    return pending ? `In its change windows (${names(w.windows)})${until}.` : `In its change windows (${names(w.windows)})${until}: it can merge now.`
  }
  const holding = names(w.holding?.length ? w.holding : w.windows)
  if (!w.opens) return `None of its change windows (${holding}) opens within a year: it does not merge until they change.`
  return pending
    ? `Outside its change windows (${holding}): once approved, the next possible merge is ${bothTimes(w.opens, w.zone)}.`
    : `Approved; waits for its change windows (${holding}): the next possible merge is ${bothTimes(w.opens, w.zone)}.`
}
