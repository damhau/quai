import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { sent } from "@/test/catalog"
import { renderApp } from "@/test/render"
import { state } from "@/test/server"

const dialog = () => screen.getByRole("dialog")
const preview = () => within(dialog()).getByRole("region", { name: "What will be committed" })

test("the inventory filters by words, type and owner, and links to each resource", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/inventory")
  expect(await screen.findByRole("link", { name: "prod/web01" })).toBeInTheDocument()
  expect(screen.getByText("Non-conforming")).toBeInTheDocument()
  await user.click(screen.getByRole("switch", { name: "Owned by me" }))
  expect(screen.queryByRole("link", { name: "dev/build-agent-01" })).not.toBeInTheDocument()
  await user.type(screen.getByRole("textbox", { name: "Search the inventory" }), "legacy")
  expect(screen.queryByRole("link", { name: "prod/web01" })).not.toBeInTheDocument()
  await user.click(screen.getByRole("link", { name: "prod/legacy" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/resources/infra-live/vm/prod/legacy"))
})

test("a resource shows its file, history and what its owner can do", async () => {
  renderApp("/resources/infra-live/vm/prod/web01")
  expect(await screen.findByRole("heading", { name: "web01" })).toBeInTheDocument()
  expect(screen.getByLabelText("Content of envs/prod/vms/web01.yaml")).toHaveTextContent("cpu: 4 # bumped in INC-4411")
  expect(screen.getByText("the existing infrastructure")).toBeInTheDocument()
  const actions = screen.getByRole("toolbar", { name: "Day-2 actions" })
  expect(within(actions).getByRole("button", { name: "Resize" })).toBeEnabled()
  expect(within(actions).getByRole("button", { name: "Restart" })).toBeDisabled() // imperative: needs an executor
})

test("someone who does not own a resource cannot change it", async () => {
  state.user = "carol"
  renderApp("/resources/infra-live/vm/prod/web01")
  expect(await screen.findByText("Only bob or an operator can change this resource.")).toBeInTheDocument()
  expect(screen.queryByRole("toolbar", { name: "Day-2 actions" })).not.toBeInTheDocument()
})

test("a resource that does not match its schema lists why and cannot be changed", async () => {
  renderApp("/resources/infra-live/vm/prod/legacy")
  expect(await screen.findByText("must be at most 32")).toBeInTheDocument()
  expect(within(screen.getByRole("toolbar", { name: "Day-2 actions" })).getByRole("button", { name: "Edit" })).toBeDisabled()
})

test("editing starts from the current settings, keeps immutable fields and previews the diff", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/resources/infra-live/vm/prod/web01")
  await user.click(await screen.findByRole("button", { name: "Edit" }))
  const cpu = within(dialog()).getByRole("spinbutton", { name: /vCPUs/ })
  expect(cpu).toHaveValue(4)
  expect(within(dialog()).getByRole("radio", { name: "ubuntu-24.04" })).toBeDisabled()
  await user.clear(cpu)
  await user.type(cpu, "8")
  expect(await within(preview()).findByText(/cpu: 8/)).toBeInTheDocument()
  expect(sent.plans.at(-1)).toMatchObject({ kind: "update", key: "prod/web01", values: { cpu: 8, template: "ubuntu-24.04" } })
  expect(sent.plans.at(-1)?.values).not.toHaveProperty("owner")
  await user.click(within(dialog()).getByRole("button", { name: "Submit for approval" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/requests/7c9e6679-7425-40de-944b-e07fc1f90ae7"))
  expect(sent.requests.at(-1)).toMatchObject({ kind: "update", key: "prod/web01" })
})

test("an action's form starts from the current values and shows the server's errors", async () => {
  const user = userEvent.setup()
  renderApp("/resources/infra-live/vm/prod/web01")
  await user.click(await screen.findByRole("button", { name: "Resize" }))
  expect(within(dialog()).getByRole("spinbutton", { name: /Memory/ })).toHaveValue(16)
  const cpu = within(dialog()).getByRole("spinbutton", { name: /vCPUs/ })
  await user.clear(cpu)
  await user.type(cpu, "64")
  expect(await within(dialog()).findByText("must be at most 32")).toBeInTheDocument()
  expect(sent.plans.at(-1)).toMatchObject({ kind: "action", action: "resize", actionInputs: { cpu: 64, memory_gb: 16 } })
})

test("deleting previews the removal and its approvals", async () => {
  const user = userEvent.setup()
  renderApp("/resources/infra-live/vm/prod/web01")
  await user.click(await screen.findByRole("button", { name: "Delete" }))
  expect(await within(preview()).findByText("type/production")).toBeInTheDocument()
  expect(within(preview()).getByTitle("delete")).toHaveTextContent("D")
  expect(within(dialog()).getByRole("button", { name: "Submit for approval" })).toBeEnabled()
})
