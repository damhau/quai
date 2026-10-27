import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { renderApp } from "@/test/render"
import { state } from "@/test/server"
import { localPath, oidcLoginURL } from "@/lib/auth"

test("the shell shows the navigation, the user and the catalog", async () => {
  renderApp("/")
  expect(await screen.findByRole("heading", { name: "Catalog" })).toBeInTheDocument()
  const nav = screen.getByRole("navigation", { name: "Main" })
  for (const label of ["Catalog", "Inventory", "Requests", "Approvals", "Designer", "Git log"]) {
    expect(nav).toHaveTextContent(label)
  }
  expect(await screen.findByText("Virtual machine (Terraform)")).toBeInTheDocument()
  expect(screen.getByText("Blueprint")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Account" })).toHaveTextContent("bob")
})

test("without a session, the login page offers the development users and comes back", async () => {
  state.user = null
  const { router } = renderApp("/inventory")
  expect(await screen.findByText("Development mode: pick a user.")).toBeInTheDocument()
  expect(router.state.location.pathname).toBe("/login")
  await userEvent.click(await screen.findByRole("button", { name: /alice/ }))
  expect(await screen.findByRole("heading", { name: "Inventory" })).toBeInTheDocument()
  expect(router.state.location.pathname).toBe("/inventory")
  expect(screen.getByRole("button", { name: "Account" })).toHaveTextContent("alice")
})

test("signing out sends the CSRF token and returns to the login page", async () => {
  const { router } = renderApp("/")
  await userEvent.click(await screen.findByRole("button", { name: "Account" }))
  await userEvent.click(await screen.findByRole("menuitem", { name: /Sign out/ }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
  expect(state.logouts).toEqual(["csrf-bob"])
})

test("a server without a database says nobody can sign in", async () => {
  state.user = null
  state.auth = "none"
  renderApp("/")
  expect(await screen.findByText(/nobody can sign in/)).toBeInTheDocument()
})

test("redirects stay on this site", () => {
  expect(localPath("/requests/42")).toBe("/requests/42")
  for (const bad of ["//evil.example.com", "https://evil.example.com", "/\\evil", 42, undefined]) {
    expect(localPath(bad)).toBe("/")
  }
  expect(oidcLoginURL("/inventory?type=vm")).toBe("/auth/login?next=%2Finventory%3Ftype%3Dvm")
})
