import { render, screen, within } from "@testing-library/react"
import type { UseQueryResult } from "@tanstack/react-query"
import { http, HttpResponse } from "msw"
import type { PlanView, RequestView, RuleView } from "@/api/generated/types.gen"
import { PlanPreview } from "@/components/plan-preview"
import { renderApp } from "@/test/render"
import { pendingVM } from "@/test/requests"
import { server, state } from "@/test/server"

const production: RuleView = { names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: ["alice"], satisfied: true }
const requireTwo: RuleView = {
  names: ["separationOfDuties/destroys"],
  approvers: ["owners"],
  min: 2,
  approvals: ["alice"],
  satisfied: false,
  overall: true,
  when: "plan.destroys > 0",
  planError: "no plan yet: two approvers until a plan shows that nothing is destroyed",
}
const separation = {
  definitionAuthors: "7d",
  since: "2026-10-20T10:00:00Z",
  commit: "9f1c2d3e4b5a69788796a5b4c3d2e1f001234567",
  authors: [{ name: "pat", commits: ["c0ffee1234567890c0ffee1234567890c0ffee12"] }],
  unmatched: ["Pia <pia@example.com>"],
}

/** serve answers the request page with the pending request changed by change. */
function serve(change: Partial<RequestView>) {
  server.use(http.get("*/api/v1/requests/:id", ({ params }) => HttpResponse.json({ ...pendingVM(), id: String(params.id), ...change })))
}

test("a request names who changed its definitions in the period before it, and identities that match no one", async () => {
  serve({ rules: [production], separation })
  renderApp(`/requests/${pendingVM().id}`)
  const section = await screen.findByRole("region", { name: "Separation of duties" })
  expect(within(section).getByText("Changed its definitions in the 7d before it was planned, so cannot approve")).toBeInTheDocument()
  expect(within(section).getByText("pat")).toBeInTheDocument()
  expect(within(section).getByText("c0ffee1")).toBeInTheDocument()
  expect(within(section).getByText("Quai had recorded no definitions when the period began: their whole history counts.")).toBeInTheDocument()
  expect(within(section).getByText(/Match no one in Quai: Pia <pia@example.com>\. They exclude no one now/)).toBeInTheDocument()
})

test("someone a rule names who cannot review the request is told why", async () => {
  state.user = "alice"
  const reason = "You changed the definitions this request was planned with (c0ffee123456) in the 7d before it: the repository's separation of duties lets someone else approve it."
  serve({ rules: [production], separation, canReview: false, cannotReview: reason })
  renderApp(`/requests/${pendingVM().id}`)
  const notice = (await screen.findByText("You cannot review this request")).closest<HTMLElement>("[role=status]")!
  expect(within(notice).getByText(reason)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
})

test("requireTwo counts different people among the other rules' approvers, or its own without them", async () => {
  serve({ rules: [production, requireTwo] })
  const page = renderApp(`/requests/${pendingVM().id}`)
  expect(await screen.findByText(/1 of 2 different people among the approvers of the other rules/)).toBeInTheDocument()
  expect(screen.getByText(/no plan yet: two approvers until a plan shows that nothing is destroyed/)).toBeInTheDocument()
  expect(screen.queryByText("owners")).not.toBeInTheDocument()
  page.unmount()

  serve({ rules: [{ ...requireTwo, approvals: [] }] })
  renderApp(`/requests/${pendingVM().id}`)
  expect(await screen.findByText(/0 of 2 different people from/)).toBeInTheDocument()
  expect(screen.getByText("owners")).toBeInTheDocument()
})

test("the timeline says when a stricter separation of duties made the request stale, and why a plan decided requireTwo", async () => {
  const at = "2026-10-28T10:00:00Z"
  serve({
    state: "stale",
    events: [
      { kind: "plan_decided", data: { decisions: [{ rules: ["separationOfDuties/destroys"], applies: true, error: "the latest plan of envs/prod failed: two approvers until a plan shows that nothing is destroyed" }] }, at },
      { kind: "stale", data: { separation: "the repository's separation of duties became stricter since the plan (a plan that destroys something needs two approvers)" }, at },
    ],
  })
  renderApp(`/requests/${pendingVM().id}`)
  const timeline = await screen.findByRole("list", { name: "Timeline" })
  expect(within(timeline).getByText(/separationOfDuties\/destroys applies \(the latest plan of envs\/prod failed/)).toBeInTheDocument()
  expect(within(timeline).getByText(/became stricter since the plan \(a plan that destroys something needs two approvers\); plan the request again$/)).toBeInTheDocument()
})

test("the plan preview says who cannot approve, and that a destroying plan needs two people", () => {
  const plan: PlanView = {
    kind: "create",
    target: "infra-live/vm/prod/web09",
    repository: "infra-live",
    commit: "abc",
    hash: "h",
    changes: [],
    rules: [{ ...production, approvals: [], satisfied: false }, { ...requireTwo, approvals: [] }],
    separation: { ...separation, from: "1234567890abcdef1234567890abcdef12345678", unmatched: [], destroys: "requireTwo" },
  }
  render(<PlanPreview ready hint="" plan={{ data: plan, isError: false, isFetching: false } as UseQueryResult<PlanView>} />)
  expect(screen.getByText(/2 different people among the approvers of the other rules/)).toBeInTheDocument()
  const section = screen.getByRole("region", { name: "Separation of duties" })
  expect(within(section).getByText("Changed its definitions in the 7d before it, so cannot approve")).toBeInTheDocument()
  expect(within(section).queryByText(/whole history/)).not.toBeInTheDocument()
})
