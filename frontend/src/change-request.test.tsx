import { screen, within } from "@testing-library/react"
import type { RuleView } from "@/api/generated/types.gen"
import { actionLabel, actorLabel, alarming } from "@/lib/audit"
import { outcomeText, supersededText } from "@/lib/change-request"
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
