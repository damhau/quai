import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { renderApp } from "@/test/render"
import { audit } from "@/test/audit"
import { state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk

const row = (table: HTMLElement, seq: number) => within(table).getByRole("cell", { name: String(seq) }).closest("tr") as HTMLElement

test("auditors read the audit log, personal data in clear unless erased, page after page", async () => {
  state.user = "dana"
  renderApp("/admin/audit")
  const table = await screen.findByRole("table", { name: "Audit events" }, slow)
  expect(screen.getByRole("navigation", { name: "Main" })).toHaveTextContent("Audit log")
  expect(screen.queryByText("The audit log is not protected")).not.toBeInTheDocument()
  expect(screen.queryByText("The audit log does not verify")).not.toBeInTheDocument()
  expect(screen.getByText(/5 events held/)).toHaveTextContent(/^Verified .+: 5 events held\. Last checkpoint at seq 5, signed .+\.$/)
  expect(screen.getByRole("link", { name: "Export" })).toHaveAttribute("href", "/api/v1/audit/export")

  const bypass = row(table, 5)
  expect(within(bypass).getByText("Merged without approval")).toBeInTheDocument()
  expect(within(bypass).getByText("Quai")).toBeInTheDocument()
  expect(within(bypass).getByRole("link", { name: "infra-live/vm/prod/web01" })).toHaveAttribute("href", "/requests/11111111-1111-1111-1111-111111111111")

  const review = row(table, 4)
  expect(within(review).getByText("alice")).toBeInTheDocument()
  expect(within(review).getByText(/signed in/)).toBeInTheDocument()
  expect(within(review).getByText("10.0.0.7")).toBeInTheDocument()
  expect(within(review).getByText("(erased)")).toBeInTheDocument()

  const submitted = row(table, 3)
  expect(within(submitted).getByText("bob (API token)")).toBeInTheDocument()
  expect(within(submitted).getByText("Black Friday traffic")).toBeInTheDocument()
  expect(within(submitted).queryByText(/signed in/)).not.toBeInTheDocument()
  expect(within(submitted).getByText("aaaaaaaaaaaa")).toHaveAttribute("title", expect.stringContaining(`hash ${"3".padStart(64, "a")}`))

  await userEvent.click(screen.getByRole("button", { name: "Older events" }))
  const login = await waitFor(() => row(table, 2))
  expect(within(login).getByText("Signed in")).toBeInTheDocument()
  expect(within(login).getAllByText("carol")).toHaveLength(2)
  expect(within(login).getByText("requester@global")).toBeInTheDocument()
  expect(audit.queries.at(-1)?.get("before")).toBe("3")
  expect(screen.queryByRole("button", { name: "Older events" })).not.toBeInTheDocument()
})

test("the filters go in the address and to the server", async () => {
  state.user = "alice"
  const { router } = renderApp("/admin/audit")
  await screen.findByRole("table", { name: "Audit events" }, slow)
  const filters = screen.getByRole("form", { name: "Filters" })
  await userEvent.selectOptions(within(filters).getByLabelText("Action"), "auth")
  await userEvent.type(within(filters).getByLabelText("Who"), "carol")
  await userEvent.type(within(filters).getByLabelText("From"), "2026-11-01")
  await userEvent.click(within(filters).getByRole("button", { name: "Filter" }))
  await waitFor(() => expect(router.state.location.search).toMatchObject({ action: "auth", actor: "carol", from: "2026-11-01" }))
  await waitFor(() => expect(audit.queries.at(-1)?.get("action")).toBe("auth"))
  const q = audit.queries.at(-1)!
  expect(q.get("actor")).toBe("carol")
  expect(new Date(q.get("since")!).getTime()).toBe(new Date("2026-11-01T00:00").getTime())
  expect(q.get("until")).toBeNull()
  const table = screen.getByRole("table", { name: "Audit events" })
  await waitFor(() => expect(within(table).queryByText("Merged without approval")).not.toBeInTheDocument())
  expect(within(table).getByText("Signed in")).toBeInTheDocument()

  // The form shows the filters in the address: after a navigation, it is a new one.
  await userEvent.click(within(screen.getByRole("form", { name: "Filters" })).getByRole("button", { name: "Clear" }))
  await waitFor(() => expect(router.state.location.search).toEqual({}))
})

test("the page says when the database does not protect the audit log", async () => {
  state.user = "alice"
  audit.protected = false
  renderApp("/admin/audit")
  expect(await screen.findByText("The audit log is not protected", {}, slow)).toBeInTheDocument()
  expect(screen.getByText(/database\.migrateURL/)).toBeInTheDocument()
})

test("the page says when the audit log does not verify", async () => {
  state.user = "alice"
  audit.verification = {
    seq: 9,
    at: "2026-11-06T12:00:00.000Z",
    verified: false,
    events: 3,
    broken: 4,
    reason: "its hash is not the hash of its content",
  }
  renderApp("/admin/audit")
  expect(await screen.findByText("The audit log does not verify", {}, slow)).toBeInTheDocument()
  expect(screen.getByText(/It breaks at seq 4: its hash is not the hash of its content/)).toBeInTheDocument()
  expect(screen.getByText(/When the chain does not verify/)).toBeInTheDocument()
})

test("before the first checkpoint and verification, the page says so", async () => {
  state.user = "alice"
  audit.verification = undefined
  audit.checkpoint = undefined
  renderApp("/admin/audit")
  expect(await screen.findByText(/Not verified yet/, {}, slow)).toHaveTextContent(
    "Not verified yet: the worker verifies the chain every day. No signed checkpoint yet.",
  )
})

test("only auditors and platform admins see the audit log in the navigation", async () => {
  renderApp("/")
  const nav = await screen.findByRole("navigation", { name: "Main" }, slow)
  await screen.findByRole("button", { name: "Account" })
  expect(within(nav).queryByRole("link", { name: /Audit log/ })).not.toBeInTheDocument()
})
