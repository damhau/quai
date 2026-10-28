import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { http, HttpResponse } from "msw"
import { sent, vm } from "@/test/catalog"
import { renderApp } from "@/test/render"
import { server } from "@/test/server"

test("catalog authors preview a branch's definitions and try a form, which cannot be submitted", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/designer")
  await user.click(await screen.findByRole("link", { name: /Preview a branch of/ }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/preview/infra-live"))

  await user.type(await screen.findByRole("textbox", { name: "Branch" }), "broken")
  await user.click(screen.getByRole("button", { name: "Preview" }))
  expect(await screen.findByText("Problems in these definitions")).toBeInTheDocument()
  expect(screen.getByText(".quai/blueprints/app-servers.yaml")).toBeInTheDocument()

  await user.clear(screen.getByRole("textbox", { name: "Branch" }))
  await user.type(screen.getByRole("textbox", { name: "Branch" }), "vm-v2")
  await user.click(screen.getByRole("button", { name: "Preview" }))
  await waitFor(() => expect(router.state.location.search).toEqual({ branch: "vm-v2" }))
  expect(await screen.findByText("Virtual machine v2")).toBeInTheDocument()
  expect(screen.queryByText("Problems in these definitions")).not.toBeInTheDocument()
  expect(screen.getByText("2a723df34bd8")).toBeInTheDocument()

  await user.click(screen.getByRole("link", { name: "Try Virtual machine v2" }))
  expect(await screen.findByRole("heading", { name: "Virtual machine v2" })).toBeInTheDocument()
  expect(screen.getByText("Preview of the branch vm-v2")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Merge the definitions to request this" })).toBeDisabled()
  expect(within(screen.getByRole("region", { name: "What will be committed" })).getByText("No plan in a preview")).toBeInTheDocument()

  // The form works, but nothing is planned or submitted.
  await user.click(screen.getByRole("radio", { name: "prod" }))
  await user.type(screen.getByRole("textbox", { name: /VM name/ }), "web01")
  await new Promise((r) => setTimeout(r, 600))
  expect(sent.plans).toEqual([])
  expect(screen.queryByRole("textbox", { name: "Reason" })).not.toBeInTheDocument()
})

test("a pull request is previewed by its ID; a missing branch says so", async () => {
  const user = userEvent.setup()
  const { router } = renderApp("/preview/infra-live")
  await user.click(await screen.findByRole("radio", { name: "Pull request" }))
  await user.type(screen.getByRole("textbox", { name: "Pull request ID" }), "42")
  await user.click(screen.getByRole("button", { name: "Preview" }))
  await waitFor(() => expect(router.state.location.search).toEqual({ pullRequest: "42" }))
  expect(await screen.findByText(/pull request #42 at/)).toBeInTheDocument()

  await router.navigate({ to: "/preview/$repository", params: { repository: "infra-live" }, search: { branch: "nope" } })
  expect(await screen.findByText(/nothing to preview/)).toBeInTheDocument()
})

test("a repository whose definitions can change without review is flagged in red", async () => {
  const problems = ["the branch main is not protected: the definitions in .quai/ can change without a pull request"]
  server.use(
    http.get("*/api/v1/catalog", () =>
      HttpResponse.json([{ kind: "type", repository: "infra-live", name: "vm", title: vm.title, category: "Compute", tags: [], trust: problems }]),
    ),
    http.get("*/api/v1/repositories/infra-live/types/vm", () => HttpResponse.json({ ...vm, trust: problems })),
  )
  const user = userEvent.setup()
  renderApp("/")
  expect(await screen.findByText("Unreviewed definitions")).toBeInTheDocument()
  await user.click(screen.getByRole("link", { name: `Request ${vm.title}` }))
  const alert = await screen.findByRole("alert")
  expect(within(alert).getByText("These definitions can change without review")).toBeInTheDocument()
  expect(within(alert).getByText(problems[0]!)).toBeInTheDocument()
})
