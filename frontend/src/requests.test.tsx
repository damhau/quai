import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { renderApp } from "@/test/render"
import { state } from "@/test/server"

const id = "11111111-1111-4111-8111-111111111111"

test("requesters see their requests; approvers see their inbox, with a count in the navigation", async () => {
  const mine = renderApp("/requests")
  expect(await screen.findByRole("link", { name: "Create infra-live/vm/prod/web09" })).toBeInTheDocument()
  expect(screen.getByText("Pending approval")).toBeInTheDocument()
  mine.unmount()

  state.user = "alice"
  renderApp("/approvals")
  const row = (await screen.findByRole("link", { name: "Create infra-live/vm/prod/web09" })).closest("tr")!
  expect(within(row).getByText("bob")).toBeInTheDocument()
  expect(await within(screen.getByRole("navigation", { name: "Main" })).findByLabelText("1 waiting")).toBeInTheDocument()
})

test("an approver reviews the exact change: denying needs a comment, approving moves the request on", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  expect(await screen.findByRole("heading", { name: "#3 Create infra-live/vm/prod/web09" })).toBeInTheDocument()
  expect(screen.getByText("cpu: 2")).toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Timeline" })).getByText("submitted the request")).toBeInTheDocument()
  expect(screen.getByText("0 of 1 from", { exact: false })).toBeInTheDocument()

  await user.click(screen.getByRole("button", { name: "Deny" }))
  expect(await screen.findByText("is required to deny: say what to change")).toBeInTheDocument()

  await user.click(screen.getByRole("button", { name: "Approve" }))
  expect(await screen.findByText("Approved", { selector: "[data-slot=badge]" })).toBeInTheDocument()
  expect(screen.getByText("Approved by alice")).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Timeline" })).getByText("all approvals are in")).toBeInTheDocument()
})

test("the requester can cancel a request that is not delivered", async () => {
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
  await user.click(await screen.findByRole("button", { name: "Cancel request" }))
  expect(await screen.findByText("Cancelled", { selector: "[data-slot=badge]" })).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Cancel request" })).not.toBeInTheDocument()
})

test("a request that does not exist says so", async () => {
  renderApp("/requests/22222222-2222-4222-8222-222222222222")
  expect(await screen.findByText("This page cannot be shown")).toBeInTheDocument()
  expect(screen.getByText("no such request")).toBeInTheDocument()
})
