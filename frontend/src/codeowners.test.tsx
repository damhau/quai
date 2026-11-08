import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { UseQueryResult } from "@tanstack/react-query"
import { http, HttpResponse } from "msw"
import type { PlanView, ResourceView } from "@/api/generated/types.gen"
import { PlanPreview } from "@/components/plan-preview"
import { detail, resources } from "@/test/inventory"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"
import { server, state } from "@/test/server"

const [web01, buildAgent] = resources as [ResourceView, ResourceView, ...ResourceView[]]

const codeowners = { owners: ["email:erin@acme.ch", "group:finops"], unmatched: ["@acme/ops"] }

test("a request approved by code owners lists them, and those that match no one", async () => {
  requests.current = { ...pendingVM(), rules: [{ names: ["type/production"], approvers: ["codeowners"], min: 1, approvals: [], satisfied: false }], codeowners }
  renderApp(`/requests/${requests.current.id}`)
  const owners = await screen.findByRole("region", { name: "Code owners" })
  expect(within(owners).getByText("group:finops")).toBeInTheDocument()
  expect(within(owners).getByText("email:erin@acme.ch")).toBeInTheDocument()
  expect(within(owners).getByText(/Match no one in Quai: @acme\/ops/)).toBeInTheDocument()
})

test("the plan preview names the code owners that will approve", () => {
  const plan: PlanView = { kind: "update", target: "infra-live/vm/prod/web01", repository: "infra-live", commit: "abc", hash: "h", changes: [], rules: [], codeowners: { owners: null, unmatched: ["@acme/dba"] } }
  render(<PlanPreview ready hint="" plan={{ data: plan, isError: false, isFetching: false } as UseQueryResult<PlanView>} />)
  const owners = screen.getByRole("region", { name: "Code owners" })
  expect(within(owners).getByText("None that Quai can match")).toBeInTheDocument()
  expect(within(owners).getByText(/Match no one in Quai: @acme\/dba/)).toBeInTheDocument()
})

test("a resource owned through its code owners names them", async () => {
  state.user = "carol"
  server.use(
    http.get("*/api/v1/resource", () => {
      const d = detail(web01, "carol")
      return HttpResponse.json({ ...d, owner: undefined, owners: ["group:finops"], unmatchedOwners: ["@acme/ops"], canOperate: false })
    }),
  )
  renderApp("/resources/infra-live/vm/prod/web01")
  expect(await screen.findByText("Only its code owners (group:finops) or an operator can change this resource.")).toBeInTheDocument()
  expect(screen.getByText(/owned by its code owners \(group:finops\)/)).toBeInTheDocument()
  expect(screen.getByText(/Match no one in Quai: @acme\/ops/)).toBeInTheDocument()
})

test("the inventory counts a resource as mine when I am one of its code owners", async () => {
  server.use(
    http.get("*/api/v1/inventory", () =>
      HttpResponse.json([
        { ...web01, owner: undefined, owners: ["group:developers"] }, // bob is a developer
        { ...buildAgent, owner: undefined, owners: ["group:platform-admins"] },
      ]),
    ),
  )
  const user = userEvent.setup()
  renderApp("/inventory")
  expect(await screen.findByRole("link", { name: "prod/web01" })).toBeInTheDocument()
  expect(screen.getByText("group:developers")).toBeInTheDocument()
  await user.click(screen.getByRole("switch", { name: "Owned by me" }))
  expect(screen.getByRole("link", { name: "prod/web01" })).toBeInTheDocument()
  expect(screen.queryByRole("link", { name: "dev/build-agent-01" })).not.toBeInTheDocument()
})
