import type { EventPreferenceInput, EventPreferenceView, PreferencesOutputBody, UpdatePreferencesInputBody } from "@/api/generated/types.gen"

// Notification preferences (docs/design/05-delivery-and-approvals.md#notifications): for each kind
// of notification and channel, at once, in the daily digest, or off. The server checks every
// choice and keeps those that differ from the defaults; alerts are not a choice.

export type Kind = EventPreferenceView["kind"]
export type Mode = EventPreferenceView["mode"]
export type Channel = EventPreferenceView["channel"]

/** kinds names each kind of notification, in the order the server lists them. */
export const kinds: Record<Kind, { label: string; description: string }> = {
  approval_needed: { label: "Approval needed", description: "A request waits for your approval" },
  submitted: { label: "Submitted", description: "Your request was submitted and waits for approvals" },
  approved: { label: "Approved", description: "Your request was approved" },
  rejected: { label: "Rejected", description: "Your request was rejected" },
  stale: { label: "Stale", description: "What your request was planned on changed: request it again" },
  merged: { label: "Merged", description: "Your request's change was merged or committed" },
  applied: { label: "Applied", description: "Your request was applied" },
  failed: { label: "Failed", description: "Delivering or applying your request failed" },
  expiring_soon: { label: "Expiring soon", description: "What your request created expires soon" },
}

export const modes: Record<Mode, string> = {
  immediate: "At once",
  digest: "In the daily digest",
  off: "Off",
}

export const channels: Record<string, string> = { email: "Email", teams: "Microsoft Teams" }

/** Choices are the form's modes, by kind and channel. */
export type Choices = Record<string, Mode>

export const choiceKey = (kind: string, channel: string) => `${kind}/${channel}`

/** choicesOf returns the modes a view shows, or its defaults. */
export function choicesOf(v: PreferencesOutputBody, defaults = false): Choices {
  const out: Choices = {}
  for (const e of v.events ?? []) out[choiceKey(e.kind, e.channel)] = defaults ? e.default : e.mode
  return out
}

/** bodyOf is what the form sends: every kind and channel, and when the digest goes. */
export function bodyOf(v: PreferencesOutputBody, choices: Choices, hour: number, timeZone: string): UpdatePreferencesInputBody {
  const events: EventPreferenceInput[] = (v.events ?? []).map((e) => ({
    kind: e.kind,
    channel: e.channel,
    mode: choices[choiceKey(e.kind, e.channel)] ?? e.mode,
  }))
  return { events, digest: { hour, timeZone } }
}

/** hours are the hours of the day a digest may go at. */
export const hours = Array.from({ length: 24 }, (_, h) => ({ value: h, label: `${String(h).padStart(2, "0")}:00` }))

/** browserTimeZone is the time zone this browser runs in, when it says. */
export function browserTimeZone(): string | undefined {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || undefined
  } catch {
    return undefined
  }
}

/** timeZones lists the IANA time zones this browser knows, with UTC and those given. */
export function timeZones(...also: (string | undefined)[]): string[] {
  let known: string[] = []
  try {
    known = Intl.supportedValuesOf("timeZone")
  } catch {
    // An older browser: the zones given only.
  }
  return [...new Set(["UTC", ...known, ...also.filter((z): z is string => !!z)])].sort((a, b) =>
    a === "UTC" ? -1 : b === "UTC" ? 1 : a.localeCompare(b),
  )
}
