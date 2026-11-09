import { screen, within } from "@testing-library/react"
import type { ChangeWindowView } from "@/api/generated/types.gen"
import { actionLabel, alarming } from "@/lib/audit"
import { eventText } from "@/lib/requests"
import { bothTimes, browserZone, localTime, windowText, zonedTime } from "@/lib/windows"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"

const id = "11111111-1111-4111-8111-111111111111"

// Monday 16 November 2026, 08:00 in New York: a zone neither the tests' machine nor CI is in.
const opens = "2026-11-16T13:00:00Z"
const zone = "America/New_York"

test("an approved request held for its change windows says when it can merge next, in the browser's time and the window's", async () => {
  const w: ChangeWindowView = {
    open: false,
    held: true,
    opens,
    zone,
    windows: ["office-hours", "year-end"],
    holding: ["office-hours"],
    reason: "Approved; waits for the change window, opens Mon 08:00 America/New_York",
  }
  requests.current = {
    ...pendingVM(),
    state: "approved",
    changeWindow: w,
    events: [
      ...(pendingVM().events ?? []),
      { kind: "approved", actor: "", data: null, at: "2026-11-14T10:00:00Z" },
      { kind: "window_waiting", actor: "", data: { reason: w.reason, opens, windows: ["office-hours"] }, at: "2026-11-14T10:00:01Z" },
    ],
  }
  renderApp(`/requests/${id}`)
  const line = await screen.findByTestId("change-window")
  expect(line).toHaveTextContent(`Approved; waits for its change windows (office-hours): the next possible merge is ${localTime(opens)} (${zonedTime(opens, zone)}).`)
  expect(zonedTime(opens, zone)).toMatch(/08:00 America\/New_York$/)
  const timeline = within(screen.getByRole("list", { name: "Timeline" }))
  expect(timeline.getByText(`waits for its change windows (office-hours): the next possible merge is ${localTime(opens)}`)).toBeInTheDocument()
})

test("a request pending approval outside its windows says when it could merge once approved; in them, until when", async () => {
  requests.current = { ...pendingVM(), changeWindow: { open: false, held: false, opens, zone, windows: ["office-hours"], holding: ["office-hours"] } }
  const pending = renderApp(`/requests/${id}`)
  expect(await screen.findByTestId("change-window")).toHaveTextContent(
    `Outside its change windows (office-hours): once approved, the next possible merge is ${bothTimes(opens, zone)}.`,
  )
  pending.unmount()

  const closes = "2026-11-16T22:00:00Z"
  requests.current = {
    ...pendingVM(),
    state: "approved",
    changeWindow: { open: true, held: false, closes, zone, windows: ["office-hours"], holding: ["office-hours"] },
    events: [...(pendingVM().events ?? []), { kind: "window_opened", actor: "", data: { closes, windows: ["office-hours"] }, at: opens }],
  }
  renderApp(`/requests/${id}`)
  expect(await screen.findByTestId("change-window")).toHaveTextContent(`In its change windows (office-hours) until ${bothTimes(closes, zone)}: it can merge now.`)
  expect(within(screen.getByRole("list", { name: "Timeline" })).getByText(`its change windows opened; they close ${localTime(closes)}`)).toBeInTheDocument()
})

test("a window that cannot be decided holds the request and says why; none opening within a year says so", () => {
  const error = "change window gold: no such key: tier"
  expect(windowText("approved", { open: false, held: true, windows: ["gold"], error, reason: `Approved; held: ${error}` })).toBe(
    `Approved, but held: its change windows cannot be decided (${error}). It does not merge until this is fixed.`,
  )
  expect(windowText("pending_approval", { open: false, held: false, windows: ["gold"], error })).toBe(
    `Its change windows cannot be decided (${error}): once approved, it waits until this is fixed.`,
  )
  expect(windowText("approved", { open: false, held: true, windows: ["never"], holding: ["never"], zone: "UTC" })).toBe(
    "None of its change windows (never) opens within a year: it does not merge until they change.",
  )
  expect(eventText({ kind: "window_waiting", actor: "", data: { error, windows: ["gold"] }, at: opens })).toBe(`held: its change windows (gold) cannot be decided: ${error}`)
  expect(eventText({ kind: "window_waiting", actor: "", data: { windows: ["never"] }, at: opens })).toBe("waits for its change windows (never): none opens within a year")
})

test("in the browser's own time zone, a time is given once", () => {
  expect(bothTimes(opens, browserZone())).toBe(localTime(opens))
  expect(bothTimes(opens)).toBe(localTime(opens))
  expect(bothTimes(opens, zone)).toBe(`${localTime(opens)} (${zonedTime(opens, zone)})`)
})

test("a merge while Quai held the request is a bypass, in the timeline and in the audit log", () => {
  expect(eventText({ kind: "merged_outside_window", actor: "", data: { commit: "4b825dc642cb6eb9a060e54bf8d69288fbee4904", pullRequest: "12" }, at: opens })).toBe(
    "the pull request was merged while Quai held it for its change windows, as 4b825dc: the enforcement was bypassed",
  )
  expect(actionLabel("request.merged_outside_window")).toBe("Merged outside its change windows")
  expect(alarming("request.merged_outside_window")).toBe(true)
  expect(actionLabel("request.window_waiting")).toBe("Waiting for a change window")
  expect(alarming("request.window_waiting")).toBe(false)
})
