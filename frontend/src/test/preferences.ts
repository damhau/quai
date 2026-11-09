import { http, HttpResponse } from "msw"
import type { EventPreferenceView, PreferencesOutputBody, UpdatePreferencesInputBody } from "@/api/generated/types.gen"
import { kinds, type Kind, type Mode } from "@/lib/preferences"

// A fake of the notification preferences: each person's, from the session (the server never takes
// a name from the body); the bodies it was sent, and a problem to answer the next save with.

type Stored = { modes: Record<string, Mode>; hour?: number; timeZone?: string }

export const preferences: {
  channels: string[]
  stored: Record<string, Stored>
  puts: { user: string; csrf: string | null; body: UpdatePreferencesInputBody }[]
  fail: { status: number; detail: string; errors?: { location: string; message: string }[] } | null
} = { channels: ["email"], stored: {}, puts: [], fail: null }

export function resetPreferences() {
  Object.assign(preferences, { channels: ["email"], stored: {}, puts: [], fail: null })
}

const defaults: Partial<Record<Kind, Mode>> = { stale: "off" }
const defaultDigest = { hour: 8, timeZone: "UTC" }

function view(user: string): PreferencesOutputBody {
  const s = preferences.stored[user] ?? { modes: {} }
  const events: EventPreferenceView[] = []
  for (const kind of Object.keys(kinds) as Kind[]) {
    for (const channel of preferences.channels) {
      const def = defaults[kind] ?? "immediate"
      events.push({
        kind,
        channel: channel as EventPreferenceView["channel"],
        mode: s.modes[kind] ?? def,
        default: def,
        modes: kind === "approval_needed" && preferences.channels.length < 2 ? ["immediate", "digest"] : ["immediate", "digest", "off"],
      })
    }
  }
  return {
    channels: preferences.channels,
    events,
    digest: { hour: s.hour ?? defaultDigest.hour, timeZone: s.timeZone ?? defaultDigest.timeZone },
    defaultDigest,
    customized: Object.keys(s.modes).length > 0 || s.hour !== undefined || s.timeZone !== undefined,
  }
}

export const preferencesHandlers = (user: () => string | null) => [
  http.get("*/api/v1/me/preferences", () => {
    const u = user()
    if (!u) return HttpResponse.json({ status: 401, detail: "not signed in" }, { status: 401 })
    return HttpResponse.json(view(u))
  }),
  http.put("*/api/v1/me/preferences", async ({ request }) => {
    const u = user()
    if (!u) return HttpResponse.json({ status: 401, detail: "not signed in" }, { status: 401 })
    const body = (await request.json()) as UpdatePreferencesInputBody
    const csrf = request.headers.get("X-CSRF-Token")
    preferences.puts.push({ user: u, csrf, body })
    if (csrf !== `csrf-${u}`) return HttpResponse.json({ status: 403, detail: "missing or wrong X-CSRF-Token header" }, { status: 403 })
    if (preferences.fail) {
      const p = preferences.fail
      preferences.fail = null
      return HttpResponse.json(p, { status: p.status, headers: { "Content-Type": "application/problem+json" } })
    }
    const s: Stored = { modes: {} }
    for (const e of body.events ?? []) if (e.mode !== (defaults[e.kind] ?? "immediate")) s.modes[e.kind] = e.mode
    if (body.digest.hour !== defaultDigest.hour) s.hour = body.digest.hour
    if (body.digest.timeZone !== defaultDigest.timeZone) s.timeZone = body.digest.timeZone
    preferences.stored[u] = s
    return HttpResponse.json(view(u))
  }),
]
