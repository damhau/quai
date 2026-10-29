import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { RequestView, RunView } from "@/api/generated/types.gen"
import { pendingVM } from "@/test/requests"
import { renderApp } from "@/test/render"
import { server } from "@/test/server"

const at = "2026-10-28T10:00:00Z"

const plan: RunView = {
  id: "r1",
  repository: "infra-live",
  stack: "envs/prod",
  commit: "abcdef1",
  pullRequest: "12",
  kind: "plan",
  status: "succeeded",
  url: "https://ci.example.com/88121",
  startedAt: at,
  finishedAt: at,
  summary: { add: 1, change: 1, destroy: 1 },
  cost: { currency: "CHF", monthlyDelta: 142.4 },
  policies: [
    { name: "vm-tags-required", result: "passed" },
    { name: "no-public-ip", result: "warning", message: "a public IP" },
  ],
  plan: {
    resources: [
      {
        resource: "infra-live/vm/prod/web01",
        type: "vm",
        key: "prod/web01",
        changes: [{ address: 'module.vm["web01"].terraform_data.vm', action: "update", attributes: ["input.cpu"], values: { "input.cpu": { before: 4, after: 8 } } }],
      },
      { resource: "infra-live/vm/prod/db01", type: "vm", key: "prod/db01", changes: [{ address: 'module.vm["db01"].terraform_data.vm', action: "delete" }] },
    ],
    unmapped: [{ address: "terraform_data.dns", action: "create" }],
  },
}

function withPlan(change: Partial<RequestView>) {
  server.use(http.get("*/api/v1/requests/:id", () => HttpResponse.json({ ...pendingVM(), plans: [plan], ...change })))
}

test("a request's plan shows what it changes by resource, what it destroys first, its cost and its policies", async () => {
  withPlan({})
  renderApp(`/requests/${pendingVM().id}`)
  const alert = await screen.findByRole("alert")
  expect(alert).toHaveTextContent("The plan destroys 1 Terraform resource")
  expect(alert).toHaveTextContent('module.vm["db01"].terraform_data.vm')
  const section = screen.getByRole("region", { name: "Plan of envs/prod" })
  expect(within(section).getByLabelText("Summary")).toHaveTextContent("+1 ~1 −1")
  expect(within(section).getByText("+CHF 142.4/month")).toBeInTheDocument()
  expect(within(section).getByText("no-public-ip: warning")).toHaveAttribute("title", "a public IP")
  expect(within(section).getByRole("link", { name: "prod/web01" })).toHaveAttribute("href", "/resources/infra-live/vm/prod/web01")
  expect(within(section).getByText(": 4 → 8")).toBeInTheDocument()
  expect(within(section).getByText("Not described by a type")).toBeInTheDocument()
  expect(within(section).getByRole("link", { name: /run/ })).toHaveAttribute("href", "https://ci.example.com/88121")
})

test("rules on the plan say whether they wait for it, apply, or do not apply", async () => {
  withPlan({
    plans: [],
    rules: [
      { names: ["type/cost"], approvers: ["group:finops"], min: 1, approvals: [], satisfied: false, waitingForPlan: true, when: "plan.cost.monthlyDelta > 500" },
      { names: ["type/destroys"], approvers: ["group:security"], min: 1, approvals: [], satisfied: true, notApplicable: true, when: "plan.destroys > 0" },
      { names: ["type/budget"], approvers: ["group:finops"], min: 1, approvals: [], satisfied: false, when: "plan.cost.monthlyDelta > 100", planError: "no such key: cost" },
    ],
    events: [{ kind: "plan_decided", data: { decisions: [{ rules: ["type/destroys"], applies: false }] }, at }],
  })
  renderApp(`/requests/${pendingVM().id}`)
  expect(await screen.findByLabelText("Waiting for the plan")).toBeInTheDocument()
  expect(screen.getByText("Decided by the plan:")).toBeInTheDocument()
  expect(screen.getByLabelText("Does not apply")).toBeInTheDocument()
  expect(screen.getByText("Does not apply to the plan:")).toBeInTheDocument()
  expect(screen.getByText(/no such key: cost/)).toBeInTheDocument()
  expect(screen.getByText("the plan decided the rules that read it: type/destroys does not apply")).toBeInTheDocument()
  expect(screen.queryByRole("region", { name: /Plan of/ })).not.toBeInTheDocument()
})

test("the preview of a request says which rules the plan decides", async () => {
  const { render, screen: s } = await import("@testing-library/react")
  const { RulesPreview } = await import("@/components/approval/rules-preview")
  render(<RulesPreview rules={[{ names: ["type/cost"], approvers: ["group:finops"], min: 1, approvals: [], satisfied: false, waitingForPlan: true, when: "plan.cost.monthlyDelta > 500" }]} />)
  expect(s.getByText("Decided by the plan:")).toBeInTheDocument()
  expect(s.getByText("plan.cost.monthlyDelta > 500")).toBeInTheDocument()
})
