import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { instantOf, localInput } from "@/lib/delegations"
import { delegations } from "@/test/delegations"
import { renderApp } from "@/test/render"
import { requests } from "@/test/requests"
import { state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk
const id = "11111111-1111-4111-8111-111111111111"
const day = 24 * 3600 * 1000

/** delegated records a delegation, as the server would have made it. */
function delegated(delegator: string, delegate: string, active = true) {
  const starts = new Date(Date.now() - (active ? 3600 * 1000 : -day))
  delegations.list.push({
    id: `00000000-0000-4000-8000-0000000000${String(delegations.list.length + 1).padStart(2, "0")}`,
    delegator,
    delegate,
    startsAt: starts.toISOString(),
    endsAt: new Date(starts.getTime() + 7 * day).toISOString(),
    createdAt: starts.toISOString(),
    active,
  })
}

test("an approver delegates their approvals to a colleague until a date, from the session, and revokes it", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  renderApp("/profile")
  expect(await screen.findByRole("heading", { name: "Delegation" }, slow)).toBeInTheDocument()
  expect(await screen.findByText("You have not delegated your approvals.")).toBeInTheDocument()
  expect(screen.getByText(/for 90 days at most; one delegate at a time/)).toBeInTheDocument()
  expect(screen.getByText(/alerts still reach you/)).toBeInTheDocument()

  await user.type(screen.getByLabelText("Colleague"), "fiona")
  const until = localInput(new Date(Date.now() + 10 * day))
  fireEvent.change(screen.getByLabelText("Until"), { target: { value: until } })
  const before = Date.now()
  await user.click(screen.getByRole("button", { name: "Delegate" }))
  await waitFor(() => expect(delegations.posts).toHaveLength(1))
  const { body, csrf, user: who } = delegations.posts[0]!
  expect(who).toBe("alice")
  expect(csrf).toBe("csrf-alice")
  // Nothing names the delegator: the server takes them from the session. An empty start is now.
  expect(Object.keys(body).sort()).toEqual(["delegate", "endsAt", "startsAt"])
  expect(body.delegate).toBe("fiona")
  expect(body.endsAt).toBe(instantOf(until))
  expect(Date.parse(body.startsAt)).toBeGreaterThanOrEqual(before - 1000)
  expect(Date.parse(body.startsAt)).toBeLessThanOrEqual(Date.now())

  const list = await screen.findByRole("list", { name: "Your delegations" })
  expect(within(list).getByText("fiona")).toBeInTheDocument()
  expect(within(list).getByText("Active")).toBeInTheDocument()

  // One delegate at a time: the server's refusal says what to do.
  await user.type(screen.getByLabelText("Colleague"), "bob")
  await user.click(screen.getByRole("button", { name: "Delegate" }))
  expect(await screen.findByText("Not delegated")).toBeInTheDocument()
  expect(screen.getByText(/revoke that delegation first/)).toBeInTheDocument()

  await user.click(within(list).getByRole("button", { name: "Revoke the delegation to fiona" }))
  expect(await screen.findByText("You have not delegated your approvals.")).toBeInTheDocument()
  expect(delegations.list).toEqual([])
})

test("the server's refusals show on their fields", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  renderApp("/profile")
  await user.type(await screen.findByLabelText("Colleague", {}, slow), "zed")
  await user.click(screen.getByRole("button", { name: "Delegate" }))
  expect(await screen.findByText(/is no one who has signed in to Quai/)).toBeInTheDocument()
  expect(screen.getByLabelText("Colleague")).toHaveAttribute("aria-invalid", "true")

  await user.clear(screen.getByLabelText("Colleague"))
  await user.type(screen.getByLabelText("Colleague"), "fiona")
  fireEvent.change(screen.getByLabelText("Until"), { target: { value: "2099-11-20T18:00" } })
  fireEvent.change(screen.getByLabelText("From"), { target: { value: "2099-01-01T08:00" } })
  await user.click(screen.getByRole("button", { name: "Delegate" }))
  expect(await screen.findByText(/a delegation lasts 90 days at most/)).toBeInTheDocument()
  expect(delegations.list).toEqual([])
})

test("with step-up configured, delegating asks for a recent sign-in first", async () => {
  state.user = "alice"
  state.stale = true
  delegations.stepUp = true
  const user = userEvent.setup()
  renderApp("/profile")
  expect(await screen.findByText("Delegating your approvals needs a recent sign-in", {}, slow)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Delegate" })).toBeDisabled()
  await user.click(screen.getByRole("button", { name: "Sign in again" }))
  await waitFor(() => expect(screen.getByRole("button", { name: "Delegate" })).toBeEnabled())
  await user.type(screen.getByLabelText("Colleague"), "fiona")
  await user.click(screen.getByRole("button", { name: "Delegate" }))
  expect(await screen.findByRole("list", { name: "Your delegations" })).toHaveTextContent("fiona")
})

test("a delegate sees what was delegated to them, and cannot revoke it", async () => {
  delegated("alice", "bob")
  renderApp("/profile")
  const list = await screen.findByRole("list", { name: "Delegated to you" }, slow)
  expect(within(list).getByText("alice")).toBeInTheDocument()
  expect(within(list).getByText(/you may review on their behalf what they could review/)).toBeInTheDocument()
  expect(within(list).queryByRole("button", { name: /Revoke/ })).not.toBeInTheDocument()
  expect(screen.getByText("You have not delegated your approvals.")).toBeInTheDocument()
})

test("a delegate reviews on the delegator's behalf: the review names them both", async () => {
  delegated("alice", "dana")
  state.user = "dana"
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  expect(await screen.findByText(/You review it on behalf of/, {}, slow)).toHaveTextContent("You review it on behalf of alice, who delegated their approvals to you.")
  expect(screen.getByText(/counts once: for alice, not for you as well/)).toBeInTheDocument()
  expect(screen.queryByLabelText("Review as")).not.toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Approve" }))
  expect(await screen.findByText(/You approved this request on behalf of alice/)).toBeInTheDocument()

  const reviews = screen.getByRole("list", { name: "Reviews" })
  expect(reviews).toHaveTextContent("dana approved on behalf of alice")
  expect(reviews).toHaveTextContent("through a delegation")
  expect(screen.getByText("Approved by dana (on behalf of alice)")).toBeInTheDocument()
  expect(screen.getByRole("list", { name: "Timeline" })).toHaveTextContent("dana approved on behalf of alice")
  expect(requests.current.reviews?.[0]).toMatchObject({ reviewer: "dana", onBehalfOf: "alice" })
})

test("someone who is an approver and a delegate chooses how they review: one person, one approval", async () => {
  delegated("alice", "fiona")
  state.user = "fiona"
  const user = userEvent.setup()
  renderApp(`/requests/${id}`)
  const as = await screen.findByLabelText("Review as", {}, slow)
  expect(within(as).getAllByRole("option").map((o) => o.textContent)).toEqual(["Yourself", "On behalf of alice"])
  await user.selectOptions(as, "alice")
  await user.click(screen.getByRole("button", { name: "Approve" }))
  expect(await screen.findByText(/You approved this request on behalf of alice/)).toBeInTheDocument()
  expect(screen.getByText("Approved by fiona (on behalf of alice)")).toBeInTheDocument()

  // Approving as herself replaces it.
  await user.selectOptions(screen.getByLabelText("Review as"), "")
  expect(screen.getByText(/a review here replaces that one. One person makes one approval/)).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Approve" }))
  await waitFor(() => expect(requests.current.reviews).toHaveLength(2))
  expect(requests.current.rules?.[0]?.approvals).toEqual([])
  expect(requests.current.rules?.[1]?.approvals).toEqual(["fiona"])
})

test("a card's link sent to a delegate confirms the decision on the delegator's behalf", async () => {
  delegated("alice", "dana")
  state.user = "dana"
  const user = userEvent.setup()
  renderApp(`/requests/${id}?link=fake.dana.approve.alice`)
  expect(await screen.findByText("Approve from your Microsoft Teams card, on alice's behalf", {}, slow)).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Confirm approval" }))
  expect(await screen.findByText(/You approved this request on behalf of alice/)).toBeInTheDocument()
  expect(requests.links).toEqual(["fake.dana.approve.alice"])
  expect(requests.current.reviews?.[0]).toMatchObject({ reviewer: "dana", onBehalfOf: "alice" })
})

test("a delegation not started yet gives nothing", async () => {
  delegated("alice", "dana", false)
  state.user = "dana"
  renderApp(`/requests/${id}`)
  expect(await screen.findByRole("heading", { name: /#3/ }, slow)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
})
