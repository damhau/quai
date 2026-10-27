import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { sent } from "@/test/catalog"
import { renderApp } from "@/test/render"

const preview = () => screen.getByRole("region", { name: "What will be committed" })

test("the catalog filters by words and category, and links to the request pages", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/")
  expect(await screen.findByText("Application servers")).toBeInTheDocument()
  await user.type(screen.getByRole("textbox", { name: "Search the catalog" }), "terraform")
  expect(screen.queryByText("Application servers")).not.toBeInTheDocument()
  await user.clear(screen.getByRole("textbox", { name: "Search the catalog" }))
  await user.click(screen.getByRole("radio", { name: "Applications" }))
  expect(screen.queryByText("Virtual machine (Terraform)")).not.toBeInTheDocument()
  await user.click(screen.getByRole("link", { name: "Request Application servers" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/request/infra-live/blueprints/app-servers"))
})

test("a resource request previews its commit and approvals, then submits", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/request/infra-live/types/vm")
  expect(await screen.findByRole("heading", { name: "Virtual machine (Terraform)" })).toBeInTheDocument()
  expect(within(preview()).getByText("Fill in the Environment and VM name to see the commit.")).toBeInTheDocument()

  // The form comes from the schema, with its defaults and without the owner, which Quai sets.
  expect(screen.getByRole("spinbutton", { name: /vCPUs/ })).toHaveValue(2)
  expect(screen.getByRole("radio", { name: "ubuntu-24.04" })).toHaveAttribute("aria-checked", "true")
  expect(screen.queryByLabelText(/^Owner/)).not.toBeInTheDocument()

  await user.click(screen.getByRole("radio", { name: "prod" }))
  await user.type(screen.getByRole("textbox", { name: /VM name/ }), "web01")
  expect(await within(preview()).findByText("envs/prod/vms/web01.yaml")).toBeInTheDocument()
  expect(within(preview()).getByText("type/production")).toBeInTheDocument()
  expect(within(preview()).getByText("group:platform-admins")).toBeInTheDocument()
  const last = sent.plans.at(-1)!
  expect(last.params).toEqual({ env: "prod", name: "web01" })
  expect(last.values).toMatchObject({ template: "ubuntu-24.04", cpu: 2, disks: [{ size_gb: 50, tier: "standard" }], network: { vlan: 110 } })
  expect(last.values).not.toHaveProperty("owner")

  await user.type(screen.getByRole("textbox", { name: "Reason" }), "Needed for the launch")
  await user.click(await screen.findByRole("button", { name: "Submit for approval" }))
  expect(await screen.findByRole("heading", { name: "#7 Create infra-live/vm/prod/web01" })).toBeInTheDocument()
  expect(router.state.location.pathname).toBe("/requests/7c9e6679-7425-40de-944b-e07fc1f90ae7")
  expect(sent.requests).toEqual([expect.objectContaining({ kind: "create", params: { env: "prod", name: "web01" }, reason: "Needed for the launch" })])
})

test("errors show on their fields: checked here as you type, and by the server's plan", async () => {
  const user = userEvent.setup()
  renderApp("/request/infra-live/types/vm")
  const name = await screen.findByRole("textbox", { name: /VM name/ })
  await user.type(name, "Bad_Name")
  await user.tab()
  expect(screen.getByText("must match [a-z][a-z0-9-]{1,30}")).toBeInTheDocument()
  await user.clear(name)
  await user.type(name, "web01")
  await user.click(screen.getByRole("radio", { name: "dev" }))

  const cpu = screen.getByRole("spinbutton", { name: /vCPUs/ })
  await user.clear(cpu)
  await user.type(cpu, "64")
  // The server's error arrives with the plan; the local one once the field loses focus.
  expect(await screen.findByText("must be at most 32")).toBeInTheDocument()
  expect(cpu).toHaveAttribute("aria-invalid", "true")
  expect(within(preview()).getByText("Fix the highlighted fields to see the preview.")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Submit and commit" })).toBeDisabled()
})

test("visibleIf hides fields as the form changes", async () => {
  const user = userEvent.setup()
  renderApp("/request/infra-live/types/vm")
  expect(await screen.findByRole("radiogroup", { name: "Backup policy" })).toBeInTheDocument()
  await user.click(screen.getByRole("switch", { name: /Backup/ }))
  expect(screen.queryByRole("radiogroup", { name: "Backup policy" })).not.toBeInTheDocument()
})

test("a blueprint request names its instance, follows its conditions and shows why it cannot be planned", async () => {
  const user = userEvent.setup()
  renderApp("/request/infra-live/blueprints/app-servers")
  expect(await screen.findByRole("spinbutton", { name: /Database data disk/ })).toHaveValue(200)
  await user.type(screen.getByRole("textbox", { name: /Application/ }), "shop")
  await user.type(screen.getByRole("textbox", { name: /Instance name/ }), "shop")
  expect(await within(preview()).findByText("envs/dev/vms/shop-db01.yaml")).toBeInTheDocument()
  expect(within(preview()).getByText(/No approval needed/)).toBeInTheDocument()

  await user.click(screen.getByRole("switch", { name: /Add a database server/ }))
  expect(screen.queryByRole("spinbutton", { name: /Database data disk/ })).not.toBeInTheDocument()
  await waitFor(() => expect(within(preview()).queryByText("envs/dev/vms/shop-db01.yaml")).not.toBeInTheDocument())

  const instance = screen.getByRole("textbox", { name: /Instance name/ })
  await user.clear(instance)
  await user.type(instance, "taken")
  expect(await within(preview()).findByText(/taken.yaml already exists/)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Submit and commit" })).toBeDisabled()
})
