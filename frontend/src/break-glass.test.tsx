import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { BreakGlassView } from "@/api/generated/types.gen"
import { actionLabel } from "@/lib/audit"
import { incidentText, reviewStatus } from "@/lib/break-glass"
import { eventText } from "@/lib/requests"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"
import { state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk
const id = "11111111-1111-4111-8111-111111111111"
const at = "2026-10-27T10:00:00Z"

/** broken is web09 merged by dave's break-glass, its review signed off by the signers given. */
function broken(...signers: string[]): BreakGlassView {
  return {
    by: "dave",
    reason: "Checkout is down: [click](https://evil.example) <b>now</b>",
    incident: "INC-2001",
    at,
    from: "pending_approval",
    changeSet: "sha256:web09",
    stepUp: true,
    open: signers.length < 2,
    closedAt: signers.length < 2 ? undefined : at,
    needed: 2,
    signOffs: signers.map((s) => ({
      signer: s,
      capacity: s === "alice" ? "platform-admin" : "auditor",
      comment: `Reviewed by ${s}.`,
      at,
      stepUp: true,
    })),
    canSignOff: false,
  }
}

test("a member of the break-glass role breaks glass from the request page: why and the incident, for the content the page shows", async () => {
  state.user = "dave"
  requests.current = {
    ...pendingVM(),
    delivery: { branch: "quai/req-3", pullRequest: "4", head: "e055bab6c12e" },
  }
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  await user.click(await screen.findByRole("button", { name: "Break glass" }, slow))
  const dialog = await screen.findByRole("dialog")
  expect(within(dialog).getByText(/without its approvals and past its change windows/)).toBeInTheDocument()
  expect(within(dialog).getByText(/Security and the owning team are alerted at once/)).toBeInTheDocument()
  expect(within(dialog).getByText(/never sent to the Git platform or in the alerts/)).toBeInTheDocument()

  // The server checks what was typed: its answers are shown by the fields.
  await user.click(within(dialog).getByRole("button", { name: "Break glass and merge" }))
  expect(await within(dialog).findAllByText("is required")).toHaveLength(2)
  expect(within(dialog).getByLabelText("Incident")).toHaveAttribute("aria-invalid", "true")

  await user.type(within(dialog).getByLabelText("Incident"), "INC-2001")
  // [[ types a [.
  await user.type(within(dialog).getByLabelText("Why it cannot wait for its approvals"), "Checkout is down: [[click](https://evil.example) <b>now</b>")
  await user.click(within(dialog).getByRole("button", { name: "Break glass and merge" }))
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
  expect(requests.breaks.at(-1)).toEqual({
    reason: "Checkout is down: [click](https://evil.example) <b>now</b>",
    incident: "INC-2001",
    changeSet: "sha256:web09",
    head: "e055bab6c12e",
  })

  const section = await screen.findByRole("region", { name: "Break-glass" })
  expect(within(section).getByText(/Break-glass by dave/)).toBeInTheDocument()
  expect(within(section).getByText(/Merged without its approvals, for incident/)).toBeInTheDocument()
  // The reason is text: its link and markup are not rendered.
  expect(within(section).getByText("Checkout is down: [click](https://evil.example) <b>now</b>")).toBeInTheDocument()
  expect(within(section).queryByRole("link")).not.toBeInTheDocument()
  expect(within(section).getByText(/Open: 0 of 2 sign-offs/)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Break glass" })).not.toBeInTheDocument()
  expect(
    within(screen.getByRole("list", { name: "Timeline" })).getByText("broke glass for incident INC-2001: it merges without its approvals"),
  ).toBeInTheDocument()
})

test("breaking glass needs a recent sign-in when auth.stepUp is configured; the requester and others are not offered it", async () => {
  state.user = "dave"
  state.stale = true
  requests.current = { ...pendingVM(), breakGlassStepUp: true }
  const user = userEvent.setup()
  const page = renderApp(`/requests/${id}`)
  await user.click(await screen.findByRole("button", { name: "Break glass" }, slow))
  const dialog = await screen.findByRole("dialog")
  expect(within(dialog).getByText("Breaking glass needs a recent sign-in")).toBeInTheDocument()
  expect(within(dialog).queryByRole("button", { name: "Break glass and merge" })).not.toBeInTheDocument()
  await user.click(within(dialog).getByRole("button", { name: "Sign in again" }))
  expect(await within(dialog).findByLabelText("Incident")).toBeInTheDocument()
  page.unmount()

  for (const name of ["bob", "alice"]) {
    state.user = name
    const other = renderApp(`/requests/${id}`)
    expect(await screen.findByRole("heading", { name: /#3/ }, slow)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Break glass" })).not.toBeInTheDocument()
    other.unmount()
  }
  // A member of the role whom something else keeps from it is told why.
  state.user = "dave"
  requests.current = {
    ...pendingVM(),
    cannotBreakGlass: "you hold a delegation of the requester's approvals: someone else breaks glass for it",
  }
  renderApp(`/requests/${id}`)
  expect(await screen.findByText(/You cannot break glass for this request: you hold a delegation/, {}, slow)).toBeInTheDocument()
}, 20000)

test("the post-incident review takes two sign-offs by its reviewers, never the breaker's, and then closes", async () => {
  requests.current = { ...pendingVM(), state: "applied", breakGlass: broken() }
  state.user = "dave"
  const user = userEvent.setup()
  const daves = renderApp(`/requests/${id}`)
  const section = await screen.findByRole("region", { name: "Break-glass" }, slow)
  expect(within(section).queryByLabelText("What the review found")).not.toBeInTheDocument()
  daves.unmount()

  state.user = "alice"
  const alices = renderApp(`/requests/${id}`)
  const mine = await screen.findByRole("region", { name: "Break-glass" }, slow)
  await user.click(within(mine).getByRole("button", { name: "Sign off the review" }))
  expect(await within(mine).findByText("is required")).toBeInTheDocument()
  await user.type(within(mine).getByLabelText("What the review found"), "The change fixed the outage.")
  await user.click(within(mine).getByRole("button", { name: "Sign off the review" }))
  expect(await within(mine).findByText(/Open: 1 of 2 sign-offs/)).toBeInTheDocument()
  const signOffs = within(mine).getByRole("list", { name: "Sign-offs" })
  expect(within(signOffs).getByText("alice")).toBeInTheDocument()
  expect(within(signOffs).getByText(/as a platform admin/)).toBeInTheDocument()
  expect(within(mine).getByText(/You cannot sign it off: you signed it off already/)).toBeInTheDocument()
  alices.unmount()

  state.user = "dana"
  renderApp(`/requests/${id}`)
  const danas = await screen.findByRole("region", { name: "Break-glass" }, slow)
  await user.type(within(danas).getByLabelText("What the review found"), "Agreed.")
  await user.click(within(danas).getByRole("button", { name: "Sign off the review" }))
  expect(await within(danas).findByText(/Closed .*: signed off by alice and dana\./)).toBeInTheDocument()
  expect(within(danas).queryByRole("button", { name: "Sign off the review" })).not.toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Timeline" })).getByText("the break-glass review is closed: two reviewers signed it off")).toBeInTheDocument()
})

test("a sign-off needs a recent sign-in when auth.stepUp is configured", async () => {
  requests.current = {
    ...pendingVM(),
    state: "applied",
    breakGlassStepUp: true,
    breakGlass: broken(),
  }
  state.user = "alice"
  state.stale = true
  renderApp(`/requests/${id}`)
  const section = await screen.findByRole("region", { name: "Break-glass" }, slow)
  expect(within(section).getByText("Signing off a break-glass review needs a recent sign-in")).toBeInTheDocument()
  expect(within(section).queryByRole("button", { name: "Sign off the review" })).not.toBeInTheDocument()
})

test("auditors and platform admins list the break-glass reviews, the open ones first; others do not see the page", async () => {
  requests.current = {
    ...pendingVM(),
    state: "applied",
    breakGlass: broken("alice"),
  }
  state.user = "dana"
  const user = userEvent.setup()
  const danas = renderApp("/admin/break-glass")
  const table = await screen.findByRole("table", { name: "Break-glass reviews" }, slow)
  expect(within(table).getByRole("link", { name: "#3" })).toHaveAttribute("href", `/requests/${id}`)
  expect(within(table).getByText("INC-2001")).toBeInTheDocument()
  expect(within(table).getByText("Open · 1 of 2")).toBeInTheDocument()
  expect(within(table).getByText("yours to sign off")).toBeInTheDocument()
  expect(
    within(screen.getByRole("navigation", { name: "Main" })).getByRole("link", {
      name: /Break-glass/,
    }),
  ).toBeInTheDocument()
  danas.unmount()

  requests.current = {
    ...pendingVM(),
    state: "applied",
    breakGlass: broken("alice", "dana"),
  }
  const closed = renderApp("/admin/break-glass")
  expect(await screen.findByText("No break-glass review is open.", {}, slow)).toBeInTheDocument()
  await user.click(screen.getByRole("radio", { name: "All" }))
  expect(await screen.findByText("Closed")).toBeInTheDocument()
  closed.unmount()

  state.user = "bob"
  renderApp("/")
  const nav = await screen.findByRole("navigation", { name: "Main" }, slow)
  expect(within(nav).queryByRole("link", { name: /Break-glass/ })).not.toBeInTheDocument()
})

test("what the timeline and the review say", () => {
  expect(
    eventText({
      kind: "break_glass",
      actor: "dave",
      data: { incident: "INC-1", from: "approved" },
      at,
    }),
  ).toBe("broke glass for incident INC-1: it merges past its change windows")
  expect(
    eventText({
      kind: "merged",
      actor: "",
      data: { commit: "0123456789abcdef", breakGlass: "dave" },
      at,
    }),
  ).toBe("the pull request was merged as 0123456, by dave's break-glass")
  expect(
    eventText({
      kind: "break_glass_signed_off",
      actor: "alice",
      data: { signOffs: 1, needed: 2 },
      at,
    }),
  ).toBe("signed off the break-glass review (1 of 2)")
  expect(
    eventText({
      kind: "break_glass_reminded",
      actor: "",
      data: { signOffs: 0, needed: 2 },
      at,
    }),
  ).toBe("reminded the reviewers that the break-glass review is open")
  expect(reviewStatus(broken("dana"))).toMatch(/^Open: 1 of 2 sign-offs; it waits for 1 more/)
})

test("with ServiceNow, the break-glass links its incident when ServiceNow knows it, and the review shows what ServiceNow did not confirm", async () => {
  const incidentUrl = "https://example.service-now.com/incident.do?sys_id=0123456789abcdef0123456789abcdef"
  requests.current = {
    ...pendingVM(),
    state: "applied",
    breakGlass: { ...broken(), incident: "INC0012345", incidentCheck: "verified", incidentUrl, incidentLink: "linked" },
  }
  state.user = "alice"
  const linked = renderApp(`/requests/${id}`)
  let section = within(await screen.findByRole("region", { name: "Break-glass" }, slow))
  expect(section.getByRole("link", { name: /INC0012345/ })).toHaveAttribute("href", incidentUrl)
  expect(section.getByText("ServiceNow knows this incident. It is linked to the request's change request.")).toBeInTheDocument()
  expect(screen.queryByTestId("incident-problem")).not.toBeInTheDocument()
  linked.unmount()

  const problem = "ServiceNow has no incident INC0077777: glass was broken for it, unverified"
  requests.current = {
    ...pendingVM(),
    state: "applied",
    breakGlass: { ...broken(), incident: "INC0077777", incidentCheck: "refuted", incidentProblem: problem },
  }
  renderApp(`/requests/${id}`)
  section = within(await screen.findByRole("region", { name: "Break-glass" }, slow))
  expect(section.queryByRole("link", { name: /INC0077777/ })).not.toBeInTheDocument()
  expect(section.getByText("ServiceNow does not confirm this incident.")).toBeInTheDocument()
  const alert = within(screen.getByTestId("incident-problem"))
  expect(alert.getByText("ServiceNow does not confirm the incident")).toBeInTheDocument()
  expect(alert.getByText(problem)).toBeInTheDocument()
})

test("what the timeline says of a break-glass's incident in ServiceNow", () => {
  expect(eventText({ kind: "break_glass", actor: "dave", data: { incident: "INC0077777", from: "pending_approval", incidentCheck: "unverified" }, at })).toBe(
    "broke glass for incident INC0077777 (not verified in ServiceNow): it merges without its approvals",
  )
  expect(eventText({ kind: "break_glass_incident", actor: "", data: { incident: "INC0077777", check: "verified" }, at })).toBe("ServiceNow confirmed incident INC0077777")
  expect(eventText({ kind: "break_glass_incident", actor: "", data: { incident: "INC0077777", check: "refuted", problem: "ServiceNow has no incident INC0077777" }, at })).toBe(
    "ServiceNow has no incident INC0077777",
  )
  expect(eventText({ kind: "incident_linked", actor: "", data: { incident: "INC0012345", changeRequest: "CHG0030001", link: "linked" }, at })).toBe(
    "linked incident INC0012345 to change request CHG0030001 in ServiceNow",
  )
  expect(eventText({ kind: "incident_linked", actor: "", data: { incident: "INC0012345", changeRequest: "CHG0030001", link: "kept" }, at })).toBe(
    "incident INC0012345 names another change request in ServiceNow: Quai left it as it is",
  )
  expect(incidentText({ ...broken(), incidentCheck: "unverified" })).toBe(
    "ServiceNow could not be asked whether this incident exists: glass was broken without it, and Quai asks again.",
  )
  expect(incidentText({ ...broken(), incidentCheck: "verified" })).toBe("ServiceNow knows this incident. Quai links it to the request's change request once it is opened.")
  expect(incidentText(broken())).toBe("")
  expect(actionLabel("request.break_glass_incident")).toBe("Break-glass incident checked in ServiceNow")
  expect(actionLabel("request.incident_linked")).toBe("Incident linked to the change request")
})
