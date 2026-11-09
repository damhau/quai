import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { renderApp } from "@/test/render"
import { access } from "@/test/access"
import { state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk

const row = (table: HTMLElement, name: string) => within(table).getByText(name, { exact: true }).closest("tr") as HTMLElement

test("auditors read who has which roles, from which groups, and download it as CSV", async () => {
  state.user = "dana"
  renderApp("/admin/access")
  const table = await screen.findByRole("table", { name: "Users" }, slow)
  expect(within(screen.getByRole("navigation", { name: "Main" })).getByRole("link", { name: /Access report/ })).toBeInTheDocument()
  expect(screen.getByRole("link", { name: "Download CSV" })).toHaveAttribute("href", "/api/v1/access-report/export")
  expect(screen.getByText(/4 people, 3 active/)).toBeInTheDocument()

  const bob = row(table, "bob")
  expect(within(bob).getByText("bob@acme.ch")).toBeInTheDocument()
  expect(within(bob).getByText("sub sub-bob")).toBeInTheDocument()
  expect(within(bob).getByText("Active")).toBeInTheDocument()
  expect(within(bob).getAllByText("developers, shop")).toHaveLength(2) // groups, and the teams of their names
  const operator = within(bob).getByText("operator").closest("li") as HTMLElement
  expect(operator).toHaveTextContent("operator · team:shop")
  expect(within(operator).getByText("from shop")).toBeInTheDocument()
  expect(within(bob).getByText(/authenticated/)).toBeInTheDocument()
  // bob's token still acts with a group he had: the report shows the role it gives.
  const tokens = within(bob).getByRole("list", { name: "API tokens of bob" })
  expect(within(tokens).getByText("ci")).toBeInTheDocument()
  expect(within(tokens).getByText("requests:write")).toBeInTheDocument()
  expect(within(tokens).getByText("platform-admin")).toBeInTheDocument()
  expect(within(tokens).getByText("from platform-admins")).toBeInTheDocument()

  // Teams through auth.teams: sam's groups are not the teams' names.
  const sam = row(table, "sam")
  expect(within(sam).getByText("sg-quai-platform, sg-quai-shop")).toBeInTheDocument()
  expect(within(sam).getByText("platform, shop")).toBeInTheDocument()
  expect(within(sam).getByText("oid 8f0c-oid")).toBeInTheDocument()

  const carol = row(table, "carol")
  expect(within(carol).getByText("Deactivated")).toBeInTheDocument()
  expect(within(carol).getByText("No role")).toBeInTheDocument()

  // Names from the identity provider are text, nothing more.
  expect(within(table).getByText('=HYPERLINK("https://evil.example.com")')).toBeInTheDocument()
  expect(access.reads).toBe(1)
})

test("the filter keeps the people with every word in their name, email, groups, teams, roles or tokens", async () => {
  state.user = "alice"
  renderApp("/admin/access")
  const table = await screen.findByRole("table", { name: "Users" }, slow)
  const filter = screen.getByLabelText("Filter")

  await userEvent.type(filter, "shop operator")
  expect(within(table).getByText("bob")).toBeInTheDocument()
  expect(within(table).queryByText("sam")).not.toBeInTheDocument()

  await userEvent.clear(filter)
  await userEvent.type(filter, "sg-quai-platform")
  expect(within(table).getByText("sam")).toBeInTheDocument()
  expect(within(table).queryByText("bob")).not.toBeInTheDocument()

  // A role a token holds counts too: bob's token is a platform admin's.
  await userEvent.clear(filter)
  await userEvent.type(filter, "platform-admin")
  expect(within(table).getByText("sam")).toBeInTheDocument()
  expect(within(table).getByText("bob")).toBeInTheDocument()

  await userEvent.clear(filter)
  await userEvent.type(filter, "deactivated")
  expect(within(table).getByText("carol")).toBeInTheDocument()
  expect(within(table).getAllByRole("row")).toHaveLength(2) // the header and carol

  await userEvent.clear(filter)
  await userEvent.type(filter, "nobody-at-all")
  expect(await screen.findByText("No one matches.")).toBeInTheDocument()
})

test("someone who is neither an auditor nor a platform admin is told so, and has no link to it", async () => {
  state.user = "bob"
  renderApp("/admin/access")
  expect(await screen.findByText("reading the access report needs the auditor role", {}, slow)).toBeInTheDocument()
  expect(screen.queryByRole("table", { name: "Users" })).not.toBeInTheDocument()
  const nav = screen.getByRole("navigation", { name: "Main" })
  await screen.findByRole("button", { name: "Account" })
  expect(within(nav).queryByRole("link", { name: /Access report/ })).not.toBeInTheDocument()
})
