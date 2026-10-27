import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { published } from "@/test/git"
import { renderApp } from "@/test/render"

test("the Git log shows a repository's commits and each commit's patch", async () => {
  const user = userEvent.setup()
  renderApp("/git")
  await user.click(await screen.findByRole("link", { name: /quai: create vm\/prod\/web01/ }))
  const commit = await screen.findByLabelText("Commit a1b2c3d")
  expect(commit).toHaveTextContent("Author: bob")
  expect(within(commit).getByText("+cpu: 2")).toBeInTheDocument()
})

test("a new blueprint is composed from the repository's types and published", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/designer")
  await user.click(await screen.findByRole("link", { name: "New blueprint" }))
  expect(await screen.findByRole("heading", { name: "New blueprint" })).toBeInTheDocument()

  await user.click(screen.getByRole("button", { name: "Add Virtual machine (Terraform)" }))
  const inspector = await screen.findByRole("group", { name: "Resource vm" })
  await user.type(await within(inspector).findByRole("textbox", { name: "Environment" }), "dev")
  await user.type(within(inspector).getByRole("textbox", { name: "VM name" }), "${{{{ inputs.name }}")
  const values = within(inspector).getByRole("textbox", { name: "Values" })
  await user.clear(values)
  await user.type(values, '{{"cpu": 2}')
  await user.tab()

  await user.click(screen.getByRole("tab", { name: "Form" }))
  await user.click(screen.getByRole("button", { name: "Add a field" }))
  await user.clear(screen.getByRole("textbox", { name: "Field 1: name" }))
  await user.type(screen.getByRole("textbox", { name: "Field 1: name" }), "name")
  await user.tab()
  await user.click(screen.getByRole("checkbox", { name: "Field 1: required" }))

  await user.click(screen.getByRole("tab", { name: "About" }))
  await user.type(screen.getByRole("textbox", { name: "Name" }), "one-vm")
  await user.type(screen.getByRole("textbox", { name: "Title" }), "One VM")
  await user.click(screen.getByRole("button", { name: "Publish" }))

  await waitFor(() => expect(router.state.location.pathname).toBe("/designer/infra-live/one-vm"))
  expect(published.blueprints[0]).toMatchObject({
    metadata: { name: "one-vm", title: "One VM" },
    spec: {
      inputs: { type: "object", properties: { name: { type: "string" } }, required: ["name"] },
      ui: { order: ["name"] },
      resources: [{ id: "vm", type: { name: "vm" }, params: { env: "dev", name: "${{ inputs.name }}" }, values: { cpu: 2 } }],
    },
  })
})

test("editing a blueprint keeps what the designer does not edit, and shows why publishing failed", async () => {
  const user = userEvent.setup()
  renderApp("/designer/infra-live/app-servers")
  const resources = await screen.findByRole("navigation", { name: "Resources" })
  await user.click(within(resources).getByRole("button", { name: "db" }))
  const inspector = screen.getByRole("group", { name: "Resource db" })
  expect(within(inspector).getByRole("textbox", { name: "Created when" })).toHaveValue("inputs.withDatabase")
  const id = within(inspector).getByRole("textbox", { name: "Resource id" })
  await user.clear(id)
  await user.type(id, "database{Enter}")
  expect(await screen.findByRole("group", { name: "Resource database" })).toBeInTheDocument()

  await user.click(screen.getByRole("button", { name: "Publish" }))
  await waitFor(() => expect(published.blueprints).toHaveLength(1))
  const spec = published.blueprints[0]!.spec as { resources: { id: string; dependsOn?: string[] }[]; actions: unknown[]; ui: unknown }
  expect(spec.resources.map((r) => r.id)).toEqual(["web", "database"])
  expect(spec.resources[0]!.dependsOn).toEqual(["database"])
  expect(spec.actions).toHaveLength(1)
  expect(spec.ui).toMatchObject({ fields: { dbDiskGb: { visibleIf: "inputs.withDatabase" } } })

  // Removing every resource makes a blueprint the server refuses.
  await user.click(screen.getByRole("button", { name: "Remove database" }))
  await user.click(within(screen.getByRole("navigation", { name: "Resources" })).getByRole("button", { name: "web" }))
  await user.click(screen.getByRole("button", { name: "Remove web" }))
  await user.click(screen.getByRole("button", { name: "Publish" }))
  expect(await screen.findByText("needs at least one resource")).toBeInTheDocument()
  expect(screen.getByText("spec.resources")).toBeInTheDocument()
})
