import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { http, HttpResponse } from "msw"
import type { PlanView } from "@/api/generated/types.gen"
import { appServers, sent } from "@/test/catalog"
import { detail, resources } from "@/test/inventory"
import { renderApp } from "@/test/render"
import { server, state } from "@/test/server"

// The rows of the schema evolution table (docs/design/02-resource-types-and-generators.md#schema-evolution),
// as the pages show them.

const slow = { timeout: 5000 } // the first render of a page loads its chunk
const dialog = () => screen.getByRole("dialog")
const legacy = resources.find((r) => r.key === "prod/legacy")!
const pin = "web: pins type vm ^1, and the type is at 2.0.0"

/** legacy, whose file the type's migration fixes, and the migration's plan. */
function migratable() {
  server.use(
    http.get("*/api/v1/resource", () => HttpResponse.json({ ...detail(legacy, state.user), migratable: true })),
    http.post("*/api/v1/plans/type", async ({ request }) => {
      const b = (await request.json()) as Record<string, unknown>
      sent.plans.push(b)
      const plan: PlanView = {
        kind: "migrate",
        target: "infra-live/vm",
        repository: "infra-live",
        type: "vm",
        commit: "abc123",
        hash: "h9",
        changes: [{ path: "envs/prod/vms/legacy.yaml", op: "update", diff: [{ op: "-", text: "cores: 4" }, { op: "+", text: "cpu: 4" }] }],
        rules: [{ names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: [], satisfied: false }],
        unmigrated: [{ key: "prod/old", reason: "tier: must be one of small, large" }],
      }
      return HttpResponse.json(plan)
    }),
  )
}

test("a new optional field, or a conforming file: no warning, and the resource can be changed", async () => {
  renderApp("/resources/infra-live/vm/prod/web01")
  expect(await screen.findByRole("heading", { name: "web01" }, slow)).toBeInTheDocument()
  expect(screen.queryByText(/does not match its schema/)).not.toBeInTheDocument()
  expect(within(screen.getByRole("toolbar", { name: "Day-2 actions" })).getByRole("button", { name: "Edit" })).toBeEnabled()
})

test("a new required field or a stricter rule: the file stays visible with its problems, edits are blocked, and nothing offers a migration", async () => {
  renderApp("/resources/infra-live/vm/prod/legacy")
  expect(await screen.findByText("The file does not match its schema; fix it in Git before Quai edits it", undefined, slow)).toBeInTheDocument()
  expect(screen.getByText("must be at most 32")).toBeInTheDocument()
  expect(screen.getByLabelText("Content of envs/prod/vms/legacy.yaml")).toBeInTheDocument()
  expect(within(screen.getByRole("toolbar", { name: "Day-2 actions" })).getByRole("button", { name: "Edit" })).toBeDisabled()
  expect(screen.queryByRole("button", { name: /Migrate/ })).not.toBeInTheDocument()
})

test("a renamed field: the type's migration is offered, as one change set over the type's non-conforming resources", async () => {
  migratable()
  const user = userEvent.setup()
  const { router } = renderApp("/resources/infra-live/vm/prod/legacy")
  expect(await screen.findByText("The file does not match its schema; the type's migration fixes it", undefined, slow)).toBeInTheDocument()
  expect(within(screen.getByRole("toolbar", { name: "Day-2 actions" })).getByRole("button", { name: "Edit" })).toBeDisabled()
  await user.click(screen.getByRole("button", { name: "Migrate every Virtual machine (Terraform)" }))
  expect(within(dialog()).getByRole("heading", { name: "Migrate every Virtual machine (Terraform)" })).toBeInTheDocument()
  expect(await within(dialog()).findByText("cpu: 4")).toBeInTheDocument()
  const left = within(dialog()).getByRole("region", { name: "Left as they are" })
  expect(left).toHaveTextContent("prod/old: tier: must be one of small, large")
  expect(sent.plans.at(-1)).toEqual({ repository: "infra-live", type: "vm", kind: "migrate" })
  await user.type(within(dialog()).getByRole("textbox", { name: /Reason/ }), "cores is now cpu")
  await user.click(within(dialog()).getByRole("button", { name: "Submit for approval" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/requests/7c9e6679-7425-40de-944b-e07fc1f90ae7"))
  expect(sent.requests.at(-1)).toEqual({ repository: "infra-live", type: "vm", kind: "migrate", reason: "cores is now cpu" })
})

test("a migration in progress is shown on the resources it fixes, and is not offered twice", async () => {
  server.use(
    http.get("*/api/v1/resource", () =>
      HttpResponse.json({ ...detail(legacy, state.user), migratable: true, requests: [{ id: "7c9e6679-7425-40de-944b-e07fc1f90ae7", number: 12, kind: "migrate", state: "pending_approval", requester: "alice", createdAt: "2026-10-31T09:00:00Z" }] }),
    ),
  )
  renderApp("/resources/infra-live/vm/prod/legacy")
  expect(await screen.findByRole("link", { name: "#12 Migrate prod/legacy" }, slow)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Migrate every Virtual machine (Terraform)" })).toBeDisabled()
})

test("someone who cannot change the resource is not offered the migration", async () => {
  migratable()
  state.user = "carol"
  renderApp("/resources/infra-live/vm/prod/legacy")
  expect(await screen.findByText("The file does not match its schema; the type's migration fixes it", undefined, slow)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: /Migrate/ })).not.toBeInTheDocument()
})

test("a major version: a blueprint that pins an older range is flagged in the catalog and on its page", async () => {
  server.use(
    http.get("*/api/v1/catalog", () =>
      HttpResponse.json([{ kind: "blueprint", repository: "infra-live", name: "app-servers", title: appServers.title, category: "Applications", tags: [], pins: [pin] }]),
    ),
    http.get("*/api/v1/repositories/infra-live/blueprints/app-servers", () => HttpResponse.json({ ...appServers, pins: [pin] })),
  )
  const user = userEvent.setup()
  renderApp("/")
  const badge = await screen.findByText("Needs an update", undefined, slow)
  expect(badge.closest("[title]")).toHaveAttribute("title", pin)
  await user.click(screen.getByRole("link", { name: `Request ${appServers.title}` }))
  const alert = await screen.findByRole("alert", undefined, slow)
  expect(alert).toHaveTextContent("This blueprint must be updated for a new version of its types")
  expect(alert).toHaveTextContent(pin)
})
