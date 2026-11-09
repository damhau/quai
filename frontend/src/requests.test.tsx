import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { browser } from "@/lib/auth"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"
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
  expect(screen.getAllByText("0 of 1 from", { exact: false })).toHaveLength(2)

  await user.click(screen.getByRole("button", { name: "Deny" }))
  expect(await screen.findByText("is required to deny: say what to change")).toBeInTheDocument()

  // The platform admins' approval is in; FinOps' is not. Alice can still change her mind.
  await user.click(screen.getByRole("button", { name: "Approve" }))
  expect(await screen.findByText(/You approved this request/)).toBeInTheDocument()
  expect(screen.getByText("Approved by alice")).toBeInTheDocument()
  expect(screen.getByText("Pending approval", { selector: "[data-slot=badge]" })).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Deny" })).toBeInTheDocument()
})

test("the last approval moves the request on, and approved requests leave the inbox", async () => {
  const user = userEvent.setup()
  state.user = "alice"
  const first = renderApp(`/requests/${id}`)
  await user.click(await screen.findByRole("button", { name: "Approve" }))
  await screen.findByText(/You approved this request/)
  first.unmount()
  const inbox = renderApp("/approvals")
  expect(await screen.findByText("Nothing needs your approval right now.")).toBeInTheDocument()
  inbox.unmount()

  state.user = "fiona"
  renderApp(`/requests/${id}`)
  await user.click(await screen.findByRole("button", { name: "Approve" }))
  expect(await screen.findByText("Approved", { selector: "[data-slot=badge]" })).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument()
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

test("a request says when it expires; expired, its approvals show as void and the timeline says why", async () => {
  requests.current = { ...pendingVM(), expiry: "7d", expiresAt: "2026-11-03T10:00:00Z" }
  const pending = renderApp(`/requests/${id}`)
  expect(await screen.findByText(/^Expires on .* unless it is approved by then \(expiry 7d\)\.$/)).toBeInTheDocument()
  pending.unmount()

  const at = "2026-11-03T10:00:30Z"
  requests.current = {
    ...requests.current,
    state: "expired",
    reviews: [{ reviewer: "alice", decision: "approve", at: "2026-10-28T09:00:00Z", stepUp: false, voided: true }],
    events: [
      ...(requests.current.events ?? []),
      { kind: "reminded", actor: "", data: { after: "48h0m0s" }, at: "2026-10-29T10:00:00Z" },
      { kind: "expired", actor: "", data: { expiry: "7d", expiresAt: "2026-11-03T10:00:00.000Z" }, at },
      { kind: "expiry_commented", actor: "", data: { pullRequest: "12" }, at },
      { kind: "pull_request_closed", actor: "", data: { pullRequest: "12", reason: "expired" }, at },
    ],
  }
  renderApp(`/requests/${id}`)
  expect(await screen.findByText("Expired", { selector: "[data-slot=badge]" })).toBeInTheDocument()
  expect(screen.getByText(/Expired: it waited for approval longer than its expiry \(7d\), so its approvals are void/)).toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Reviews" })).getByText(/void: the request expired/)).toBeInTheDocument()
  const timeline = within(screen.getByRole("list", { name: "Timeline" }))
  expect(timeline.getByText("reminded its approvers, 48h0m0s after it was submitted")).toBeInTheDocument()
  expect(timeline.getByText("expired: it waited for approval longer than its expiry (7d); its approvals are void")).toBeInTheDocument()
  expect(timeline.getByText("closed pull request #12")).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Cancel request" })).not.toBeInTheDocument()
})

test("a request that does not exist says so", async () => {
  renderApp("/requests/22222222-2222-4222-8222-222222222222")
  expect(await screen.findByText("This page cannot be shown")).toBeInTheDocument()
  expect(screen.getByText("no such request")).toBeInTheDocument()
})

test("a request that needs a step-up asks the approver to sign in again before approve works, then comes back to the decision", async () => {
  requests.current = { ...pendingVM(), stepUp: true }
  state.user = "alice"
  state.stale = true
  const user = userEvent.setup()
  const { router } = renderApp(`/requests/${id}`)
  expect(await screen.findByText("Reviewing this request needs a recent sign-in")).toBeInTheDocument()
  expect(screen.getByText("you authenticated more than 5m0s ago; sign in again")).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument()

  // The development switcher signs alice in again, here.
  await user.click(screen.getByRole("button", { name: "Sign in again" }))
  expect(await screen.findByText(/This request needs a recent sign-in: yours counts until/)).toBeInTheDocument()
  expect(router.state.location.pathname).toBe(`/requests/${id}`)
  await user.click(screen.getByRole("button", { name: "Approve" }))
  expect(await screen.findByText(/You approved this request/)).toBeInTheDocument()
  expect(screen.getByText(/signed in recently/, { selector: "span" })).toBeInTheDocument()
})

test("the server decides: an approval it refuses for an old sign-in brings the prompt back", async () => {
  requests.current = { ...pendingVM(), stepUp: true }
  state.user = "alice"
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  const approve = await screen.findByRole("button", { name: "Approve" })
  // While the page is open, alice's sign-in grows old: the page does not know, the server does.
  state.stale = true
  await user.click(approve)
  expect(await screen.findByText("Reviewing this request needs a recent sign-in")).toBeInTheDocument()
  expect(requests.current.reviews).toEqual([])
})

test("with the identity provider, signing in again leaves for it and comes back to the request", async () => {
  requests.current = { ...pendingVM(), stepUp: true }
  state.user = "alice"
  state.auth = "oidc"
  state.stale = true
  const assign = vi.spyOn(browser, "assign").mockImplementation(() => {})
  try {
    const user = userEvent.setup()
    renderApp(`/requests/${id}`)
    await user.click(await screen.findByRole("button", { name: "Sign in again" }))
    await waitFor(() => expect(assign).toHaveBeenCalledWith(`/auth/login?stepUp=1&next=${encodeURIComponent(`/requests/${id}`)}`))
  } finally {
    assign.mockRestore()
  }
})

test("a request without step-up takes the review as before", async () => {
  state.user = "alice"
  state.stale = true
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  await user.click(await screen.findByRole("button", { name: "Approve" }))
  expect(await screen.findByText(/You approved this request/)).toBeInTheDocument()
  expect(screen.queryByText(/recent sign-in/)).not.toBeInTheDocument()
})

test("Approve on a Teams card opens the request with the approval ready: confirming it reviews with the link", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  const { router } = renderApp(`/requests/${id}?link=fake.alice.approve`)
  expect(await screen.findByText("Approve from your Microsoft Teams card")).toBeInTheDocument()
  expect(screen.getByText(/nothing is decided until you do/)).toBeInTheDocument()
  // Opening the link decided nothing.
  expect(requests.current.reviews).toEqual([])
  expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Confirm approval" }))
  expect(await screen.findByText(/You approved this request/)).toBeInTheDocument()
  expect(requests.links).toEqual(["fake.alice.approve"])
  // Used, the link leaves the page: it no longer offers its decision.
  await waitFor(() => expect(router.state.location.search).toEqual({}))
})

test("Deny on a Teams card asks why before it is confirmed", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  renderApp(`/requests/${id}?link=fake.alice.deny`)
  expect(await screen.findByText("Deny from your Microsoft Teams card")).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Confirm denial" }))
  expect(await screen.findByText("is required to deny: say what to change")).toBeInTheDocument()
  await user.type(screen.getByLabelText("Why you deny it"), "Too large for the launch")
  await user.click(screen.getByRole("button", { name: "Confirm denial" }))
  expect(await screen.findByText("Denied", { selector: "[data-slot=badge]" })).toBeInTheDocument()
  expect(requests.links).toEqual(["fake.alice.deny"])
})

test("a card's link sent to someone else is refused: the approver reviews as without it", async () => {
  state.user = "fiona"
  const user = userEvent.setup()
  renderApp(`/requests/${id}?link=fake.alice.approve`)
  expect(await screen.findByText("This link from chat cannot be used")).toBeInTheDocument()
  expect(screen.getByText(/this link was sent to someone else: review the request here as yourself/)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Confirm approval" })).not.toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Approve" }))
  expect(await screen.findByText(/You approved this request/)).toBeInTheDocument()
  expect(requests.links).toEqual([undefined])
})

test("someone who cannot review is told why a card's link does not open a decision", async () => {
  const bob = renderApp(`/requests/${id}?link=fake.alice.approve`)
  expect(await screen.findByText(/this link was sent to someone else/)).toBeInTheDocument()
  bob.unmount()
  renderApp(`/requests/${id}?link=fake.expired`)
  expect(await screen.findByText(/this link expired/)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: /Approve|Confirm/ })).not.toBeInTheDocument()
})

test("a card's link to a request that needs a step-up comes back with the link after signing in again", async () => {
  requests.current = { ...pendingVM(), stepUp: true }
  state.user = "alice"
  state.stale = true
  const user = userEvent.setup()
  renderApp(`/requests/${id}?link=fake.alice.approve`)
  expect(await screen.findByText("Approve from your Microsoft Teams card")).toBeInTheDocument()
  expect(screen.getByText("Reviewing this request needs a recent sign-in")).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Confirm approval" })).not.toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Sign in again" }))
  await user.click(await screen.findByRole("button", { name: "Confirm approval" }))
  expect(await screen.findByText(/You approved this request/)).toBeInTheDocument()
  expect(requests.links).toEqual(["fake.alice.approve"])
})

test("with the identity provider, the step-up keeps the card's link to come back to", async () => {
  requests.current = { ...pendingVM(), stepUp: true }
  state.user = "alice"
  state.auth = "oidc"
  state.stale = true
  const assign = vi.spyOn(browser, "assign").mockImplementation(() => {})
  try {
    const user = userEvent.setup()
    renderApp(`/requests/${id}?link=fake.alice.approve`)
    await user.click(await screen.findByRole("button", { name: "Sign in again" }))
    await waitFor(() => expect(assign).toHaveBeenCalledWith(`/auth/login?stepUp=1&next=${encodeURIComponent(`/requests/${id}?link=fake.alice.approve`)}`))
  } finally {
    assign.mockRestore()
  }
})
