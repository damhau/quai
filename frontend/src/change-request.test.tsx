import { screen, within } from "@testing-library/react"
import type { RuleView } from "@/api/generated/types.gen"
import { actionLabel, actorLabel, alarming } from "@/lib/audit"
import { closeText, closedEventText, outcomeText, supersededText } from "@/lib/change-request"
import { eventText } from "@/lib/requests"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"

const id = "11111111-1111-4111-8111-111111111111"
const at = "2026-11-09T10:00:00Z"
const url = "https://example.service-now.com/change_request.do?sys_id=0123456789abcdef0123456789abcdef"
const ticket: RuleView = { names: ["type/change-ticket"], approvers: [], min: 0, external: "servicenow", approvals: [], satisfied: false }

test("a request that waits on ServiceNow links its change request, says what ServiceNow reported and what it means", async () => {
  requests.current = {
    ...pendingVM(),
    rules: [...(pendingVM().rules ?? []), ticket],
    changeRequest: { number: "CHG0030001", kind: "normal", state: "Assess", approval: "requested", outcome: "pending", url, polledAt: at, superseded: 1 },
    events: [
      ...(pendingVM().events ?? []),
      { kind: "change_request_superseded", actor: "", data: { changeRequest: "CHG0030001", changeSet: "sha256:old" }, at },
      { kind: "change_request_opened", actor: "", data: { changeRequest: "CHG0030002", kind: "normal", url }, at },
    ],
  }
  renderApp(`/requests/${id}`)
  const panel = within(await screen.findByTestId("change-request"))
  expect(panel.getByRole("link", { name: /CHG0030001/ })).toHaveAttribute("href", url)
  expect(panel.getByText("Normal change")).toBeInTheDocument()
  expect(panel.getByText(/Assess, approval requested/)).toBeInTheDocument()
  expect(panel.getByText("Waits for its approval in ServiceNow: Quai reads it every few minutes, and when this page opens.")).toBeInTheDocument()
  expect(panel.getByText("The change request of earlier content no longer counts: an approval is for the content it was given for.")).toBeInTheDocument()
  const timeline = within(screen.getByRole("list", { name: "Timeline" }))
  expect(timeline.getByText("opened change request CHG0030002 in ServiceNow")).toBeInTheDocument()
  expect(timeline.getByText("change request CHG0030001 no longer counts: the content changed, and a new one is opened for it")).toBeInTheDocument()
})

test("while ServiceNow cannot be reached, the page says why and that nothing is approved; what ServiceNow refused, who must act", async () => {
  requests.current = {
    ...pendingVM(),
    rules: [...(pendingVM().rules ?? []), ticket],
    changeRequest: { outcome: "pending", failure: "the ServiceNow token: EOF", failingSince: at, kind: "standard" },
  }
  const down = renderApp(`/requests/${id}`)
  let panel = within(await screen.findByTestId("change-request"))
  expect(panel.getByText(/Quai cannot reach ServiceNow since/)).toBeInTheDocument()
  expect(panel.getByText("It tries again; the request waits, and nothing is approved meanwhile.")).toBeInTheDocument()
  expect(panel.getByText("the ServiceNow token: EOF")).toBeInTheDocument()
  expect(panel.getByText("Quai is opening it in ServiceNow.")).toBeInTheDocument()
  down.unmount()

  const problem = "ServiceNow did not take CHG0030003 to scheduled: <b>Invalid state transition</b>"
  requests.current = { ...pendingVM(), changeRequest: { number: "CHG0030003", kind: "standard", state: "New", outcome: "pending", problem } }
  renderApp(`/requests/${id}`)
  panel = within(await screen.findByTestId("change-request"))
  expect(panel.getByText("Someone must act in ServiceNow")).toBeInTheDocument()
  expect(panel.getByText(problem)).toBeInTheDocument() // as text, never markup
  expect(panel.getByText("Standard change")).toBeInTheDocument()
})

test("ServiceNow's decisions read as such in the timeline and the audit log", () => {
  expect(eventText({ kind: "approved", actor: "", data: { changeRequest: "CHG0030001", system: "servicenow" }, at })).toBe(
    "all approvals are in: ServiceNow approved change request CHG0030001",
  )
  expect(eventText({ kind: "rejected", actor: "ServiceNow (CHG0030001)", data: { changeRequest: "CHG0030001", system: "servicenow", outcome: "cancelled" }, at })).toBe(
    "rejected the request: its change request was canceled in ServiceNow",
  )
  expect(eventText({ kind: "rejected", actor: "alice", data: null, at })).toBe("denied the request")
  expect(outcomeText({ outcome: "approved" }, true)).toBe("Approved in ServiceNow: the rule that waits on it is satisfied.")
  expect(outcomeText({ outcome: "closed" }, false)).toBe("Closed in ServiceNow before the request merged: the request is rejected.")
  expect(outcomeText({ outcome: "pending", number: "CHG0030001" }, false)).toBe("Quai no longer waits on it.")
  expect(supersededText(2)).toBe("The 2 change requests of earlier content no longer count: an approval is for the content it was given for.")
  expect(actionLabel("request.change_request_opened")).toBe("Change request opened in ServiceNow")
  expect(actionLabel("request.approval_withdrawn")).toBe("Approval withdrawn in ServiceNow")
  expect(alarming("request.change_request_superseded")).toBe(false)
  expect(actorLabel({ type: "system", id: "servicenow" }, {})).toBe("ServiceNow")
  expect(actorLabel({ type: "system", id: "quai" }, {})).toBe("Quai")
})

test("once applied, the change request is closed with the evidence; the page lists the files, and what ServiceNow refused", async () => {
  const files = [
    { name: "quai-evidence-3-0123456789abcdef.json", bytes: 8726, sha256: "e84d072355740808f106e0c8e8b1a4b65820b7f3672fd8aacfc5ca2149ef9c10" },
    { name: "quai-evidence-3-0123456789abcdef.pdf", bytes: 4591, sha256: "0e77ee3b9ab6668d81698fded51869ee44a566fb97f9414a3a796948ed1a521d" },
  ]
  requests.current = {
    ...pendingVM(),
    state: "applied",
    changeRequest: {
      number: "CHG0030001",
      kind: "normal",
      state: "Closed",
      approval: "approved",
      outcome: "approved",
      url,
      closeOutcome: "successful",
      closeResult: "closed",
      closedAt: at,
      attachments: files,
    },
    events: [
      ...(pendingVM().events ?? []),
      { kind: "change_request_closed", actor: "", data: { changeRequest: "CHG0030001", outcome: "successful", result: "closed", files: files.map((f) => f.name) }, at },
    ],
  }
  const closed = renderApp(`/requests/${id}`)
  let panel = within(await screen.findByTestId("change-request"))
  expect(panel.getByText("Closed in ServiceNow by Quai, successful: its content was applied.")).toBeInTheDocument()
  const list = within(panel.getByRole("list", { name: "Evidence attached" }))
  expect(list.getByText(/quai-evidence-3-0123456789abcdef\.json/)).toBeInTheDocument()
  expect(list.getByText(/8\.5 KiB, sha256 e84d07235574/)).toBeInTheDocument()
  expect(
    within(screen.getByRole("list", { name: "Timeline" })).getByText("closed change request CHG0030001 in ServiceNow, successful, the evidence attached"),
  ).toBeInTheDocument()
  closed.unmount()

  // Closing, ServiceNow unreachable: the request itself is done.
  requests.current = {
    ...pendingVM(),
    state: "failed",
    changeRequest: { number: "CHG0030002", kind: "normal", outcome: "approved", closeOutcome: "unsuccessful", failure: "ServiceNow: 503", failingSince: at },
  }
  const closing = renderApp(`/requests/${id}`)
  panel = within(await screen.findByTestId("change-request"))
  expect(panel.getByText("Quai is closing it, as its apply failed: it attaches the evidence, then moves it to Closed.")).toBeInTheDocument()
  expect(panel.getByText("It tries again to close the change request; the request itself is done.")).toBeInTheDocument()
  closing.unmount()

  const problem = "ServiceNow did not take CHG0030003 to implement: <b>Invalid state transition</b>"
  requests.current = {
    ...pendingVM(),
    state: "applied",
    changeRequest: { number: "CHG0030003", kind: "normal", state: "Scheduled", outcome: "approved", closeOutcome: "successful", closeResult: "refused", closedAt: at, problem, attachments: files },
  }
  renderApp(`/requests/${id}`)
  panel = within(await screen.findByTestId("change-request"))
  expect(panel.getByText("Not closed: ServiceNow refused a step of closing it, as its content was applied.")).toBeInTheDocument()
  expect(panel.getByText("Someone must act in ServiceNow")).toBeInTheDocument()
  expect(panel.getByText(problem)).toBeInTheDocument() // as text, never markup
})

test("what the timeline and the audit log say of closing", () => {
  expect(closeText({ outcome: "pending", closeOutcome: "successful", closedAt: at, closeResult: "left" })).toBe(
    "Left to its change manager, as its content was applied: it was never approved in ServiceNow, so Quai added a work note instead of closing it.",
  )
  expect(closeText({ outcome: "approved", closeOutcome: "unsuccessful", closedAt: at, closeResult: "already" })).toBe(
    "It was closed or canceled in ServiceNow already: Quai left it as it is, as its apply failed.",
  )
  expect(closeText({ outcome: "approved" })).toBe("")
  expect(closedEventText("CHG0030001", "unsuccessful", "closed", 2, undefined)).toBe("closed change request CHG0030001 in ServiceNow, unsuccessful: the apply failed, the evidence attached")
  expect(closedEventText("CHG0030001", "successful", "left", 2, undefined)).toBe(
    "left change request CHG0030001 to its change manager: it was never approved in ServiceNow, the evidence attached",
  )
  expect(closedEventText("CHG0030001", "successful", "refused", 0, "ServiceNow did not take CHG0030001 to review")).toBe(
    "could not close change request CHG0030001: ServiceNow did not take CHG0030001 to review",
  )
  expect(actionLabel("request.change_request_closed")).toBe("Change request closed or left, with the evidence")
})
