import { render, screen, within } from "@testing-library/react"
import type { UseQueryResult } from "@tanstack/react-query"
import type { PlanView } from "@/api/generated/types.gen"
import { PlanPreview } from "@/components/plan-preview"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"

const managerRule = { names: ["type/production"], approvers: ["manager"], min: 1, approvals: [], satisfied: false }

test("a request approved by the requester's manager names the manager when it was submitted", async () => {
  requests.current = { ...pendingVM(), rules: [managerRule], manager: { name: "mia" } }
  renderApp(`/requests/${requests.current.id}`)
  const manager = await screen.findByRole("region", { name: "Manager" })
  expect(within(manager).getByText("The requester's manager when it was submitted")).toBeInTheDocument()
  expect(within(manager).getByText("mia")).toBeInTheDocument()
})

test("a request without a manager says why nobody can approve as the manager", async () => {
  requests.current = { ...pendingVM(), rules: [managerRule], manager: { reason: "bob's manager has not signed in to Quai yet" } }
  renderApp(`/requests/${requests.current.id}`)
  const manager = await screen.findByRole("region", { name: "Manager" })
  expect(within(manager).getByText("No manager, so nobody can satisfy a manager rule: bob's manager has not signed in to Quai yet.")).toBeInTheDocument()
})

test("a request without a manager rule shows no manager", async () => {
  requests.current = pendingVM()
  renderApp(`/requests/${requests.current.id}`)
  expect((await screen.findAllByText("Approval")).length).toBeGreaterThan(0)
  expect(screen.queryByRole("region", { name: "Manager" })).not.toBeInTheDocument()
})

test("the plan preview names the manager who will approve, or why there is none", () => {
  const plan: PlanView = { kind: "create", target: "infra-live/vm/prod/web09", repository: "infra-live", commit: "abc", hash: "h", changes: [], rules: [], manager: { name: "mia" } }
  const preview = (p: PlanView) => <PlanPreview ready hint="" plan={{ data: p, isError: false, isFetching: false } as UseQueryResult<PlanView>} />
  const { rerender } = render(preview(plan))
  expect(within(screen.getByRole("region", { name: "Manager" })).getByText("mia")).toBeInTheDocument()
  rerender(preview({ ...plan, manager: { reason: "Quai knows no manager for bob: Microsoft Graph has no manager for them" } }))
  expect(screen.getByText(/Microsoft Graph has no manager for them\.$/)).toBeInTheDocument()
})
