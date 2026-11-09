import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { renderApp } from "@/test/render"
import { state } from "@/test/server"
import { localPath, oidcLoginURL, stepUpLoginURL } from "@/lib/auth"

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
  for (const good of ["/requests/42", "/requests/42?review=approve", "/catalog/vm#prod", "/resources/infra-live%2Fvm%2Fprod%2Fweb-01"]) {
    expect(localPath(good)).toBe(good)
  }
  const tab = String.fromCharCode(9)
  const del = String.fromCharCode(0x7f)
  for (const bad of [
    "//evil.example.com",
    "///evil.example.com",
    "https://evil.example.com",
    "javascript:alert(1)",
    "requests/42",
    "",
    "/\\evil",
    "/x\\y",
    `/${tab}/evil.example.com`,
    `/x${del}`,
    "/%2F/evil.example.com",
    "/%2f%2fevil.example.com",
    "/%5Cevil.example.com",
    "/%09/evil.example.com",
    "/%252F/evil.example.com",
    "/%25252F%25252Fevil.example.com",
    "/%zz",
    `/${"a".repeat(2048)}`,
    42,
    undefined,
  ]) {
    expect(localPath(bad), String(bad)).toBe("/")
  }
  expect(oidcLoginURL("/inventory?type=vm")).toBe("/auth/login?next=%2Finventory%3Ftype%3Dvm")
  expect(stepUpLoginURL("/requests/42")).toBe("/auth/login?stepUp=1&next=%2Frequests%2F42")
  expect(stepUpLoginURL("//evil.example.com")).toBe("/auth/login?stepUp=1&next=%2F")
})

test("signed in already, the login page switches to another user", async () => {
  const { router } = renderApp("/login?next=%2Fapprovals")
  await userEvent.click(await screen.findByRole("button", { name: /alice/ }))
  expect(await screen.findByRole("heading", { name: "Approvals" })).toBeInTheDocument()
  expect(router.state.location.pathname).toBe("/approvals")
  expect(screen.getByRole("button", { name: "Account" })).toHaveTextContent("alice")
})
