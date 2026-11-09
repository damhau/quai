import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { renderApp } from "@/test/render"
import { preferences } from "@/test/preferences"
import { state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk

const options = (select: HTMLElement) => within(select).getAllByRole("option").map((o) => o.textContent)

test("people choose how each notification reaches them, and when their digest goes", async () => {
  const user = userEvent.setup()
  renderApp("/profile")
  const table = await screen.findByRole("table", { name: "Notification preferences" }, slow)
  expect(screen.getByRole("heading", { name: "Profile" })).toBeInTheDocument()
  expect(screen.getByText("bob · bob@acme.ch")).toBeInTheDocument()
  expect(within(table).getAllByRole("combobox")).toHaveLength(10)
  expect(screen.getByText(/Alerts to the administrators of a repository always go at once/)).toBeInTheDocument()

  // The default is marked; approvals are never off with one channel.
  const stale = screen.getByRole("combobox", { name: "Stale by Email" })
  expect(stale).toHaveValue("off")
  expect(options(stale)).toEqual(["At once", "In the daily digest", "Off (default)"])
  expect(options(screen.getByRole("combobox", { name: "Approval needed by Email" }))).toEqual(["At once (default)", "In the daily digest"])
  expect(screen.getByText(/Approvals cannot be turned off/)).toBeInTheDocument()

  await user.selectOptions(screen.getByRole("combobox", { name: "Approved by Email" }), "digest")
  await user.selectOptions(screen.getByRole("combobox", { name: "Merged by Email" }), "off")
  await user.selectOptions(screen.getByLabelText("Send it at"), "7")
  await user.selectOptions(screen.getByLabelText("Time zone"), "Europe/Zurich")
  await user.click(screen.getByRole("button", { name: "Save" }))

  await vi.waitFor(() => expect(preferences.puts).toHaveLength(1))
  const { body, csrf, user: who } = preferences.puts[0]!
  expect(who).toBe("bob")
  expect(csrf).toBe("csrf-bob")
  // Every kind and channel, nothing that names a person: the server takes them from the session.
  expect(Object.keys(body).sort()).toEqual(["digest", "events"])
  expect(body.events).toHaveLength(10)
  expect(body.events).toContainEqual({ kind: "approved", channel: "email", mode: "digest" })
  expect(body.events).toContainEqual({ kind: "merged", channel: "email", mode: "off" })
  expect(body.events).toContainEqual({ kind: "stale", channel: "email", mode: "off" })
  expect(body.digest).toEqual({ hour: 7, timeZone: "Europe/Zurich" })
  expect(preferences.stored.bob).toEqual({ modes: { approved: "digest", merged: "off" }, hour: 7, timeZone: "Europe/Zurich" })

  // Back to the defaults, saved.
  await user.click(screen.getByRole("button", { name: "Back to the defaults" }))
  expect(screen.getByRole("combobox", { name: "Approved by Email" })).toHaveValue("immediate")
  expect(screen.getByLabelText("Send it at")).toHaveValue("8")
  expect(screen.getByLabelText("Time zone")).toHaveValue("UTC")
  await user.click(screen.getByRole("button", { name: "Save" }))
  await vi.waitFor(() => expect(preferences.puts).toHaveLength(2))
  expect(preferences.stored.bob).toEqual({ modes: {} })
})

test("the server's refusal is shown, field by field", async () => {
  const user = userEvent.setup()
  preferences.fail = {
    status: 422,
    detail: "some preferences need attention",
    errors: [{ location: "body.digest.timeZone", message: '"Mars/Olympus_Mons" is not a known time zone, such as Europe/Zurich' }],
  }
  renderApp("/profile")
  await screen.findByRole("table", { name: "Notification preferences" }, slow)
  await user.click(screen.getByRole("button", { name: "Save" }))
  const alert = await screen.findByText("Not saved")
  expect(alert.closest("[role=alert]")).toHaveTextContent(/some preferences need attention.*Mars\/Olympus_Mons/)
  expect(preferences.stored.bob).toBeUndefined()
})

test("someone without a verified email is told nothing reaches them", async () => {
  state.user = "dana"
  renderApp("/profile")
  await screen.findByRole("table", { name: "Notification preferences" }, slow)
  expect(screen.getByText(/gives Quai no verified email of yours/)).toBeInTheDocument()
})

test("Slack is a channel of its own, which finds people by their email too", async () => {
  preferences.channels = ["email", "slack"]
  state.user = "dana"
  renderApp("/profile")
  const table = await screen.findByRole("table", { name: "Notification preferences" }, slow)
  expect(within(table).getByRole("columnheader", { name: "Slack" })).toBeInTheDocument()
  expect(within(table).getAllByRole("combobox")).toHaveLength(20)
  // With two channels, approvals may be off on one.
  expect(options(screen.getByRole("combobox", { name: "Approval needed by Slack" }))).toEqual(["At once (default)", "In the daily digest", "Off"])
  expect(screen.getByText(/no email or Slack message reaches you/)).toBeInTheDocument()
})

test("without a channel, there is nothing to choose", async () => {
  preferences.channels = []
  renderApp("/profile")
  expect(await screen.findByText(/Quai sends no notifications: no channel is configured/, {}, slow)).toBeInTheDocument()
  expect(screen.queryByRole("table", { name: "Notification preferences" })).not.toBeInTheDocument()
})

test("the account menu leads to the profile", async () => {
  const user = userEvent.setup()
  renderApp("/")
  await user.click(await screen.findByRole("button", { name: "Account" }, slow))
  await user.click(await screen.findByRole("menuitem", { name: "Profile" }))
  expect(await screen.findByRole("heading", { name: "Profile" }, slow)).toBeInTheDocument()
})
