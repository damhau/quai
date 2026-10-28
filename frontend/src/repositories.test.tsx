import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { http, HttpResponse } from "msw"
import { renderApp } from "@/test/render"
import { repos } from "@/test/repositories"
import { server, state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk

test("the repositories page lists every connection with its health, for admins only", async () => {
  state.user = "alice"
  server.use(
    http.get("*/api/v1/repositories", () =>
      HttpResponse.json([
        {
          name: "infra-live",
          title: "Infrastructure",
          connector: "gitea",
          managedBy: "config",
          branch: "main",
          delivery: "pull-request",
          commit: "abc",
          types: 1,
          blueprints: 0,
          problems: [],
          trust: { checked: true, problems: [] },
          enforcement: { required: true, checked: true, problems: [] },
        },
        {
          name: "dns",
          title: "DNS",
          connector: "azure-devops",
          managedBy: "api",
          branch: "main",
          delivery: "pull-request",
          commit: "def",
          types: 1,
          blueprints: 0,
          problems: [],
          trust: { checked: true, problems: [] },
          enforcement: { required: true, checked: true, problems: ["no status check policy on main requires quai/approval"] },
        },
      ]),
    ),
  )
  renderApp("/admin/repositories")
  const table = await screen.findByRole("table", { name: "Repositories" }, slow)
  const row = (name: string) => within(table).getByRole("link", { name }).closest("tr") as HTMLElement
  await waitFor(() => expect(within(row("DNS")).getByText("Approvals not enforced")).toBeInTheDocument())
  expect(within(row("Infrastructure")).getByText("Healthy")).toBeInTheDocument()
  expect(within(row("Infrastructure")).getByText("Configuration")).toBeInTheDocument()
  expect(within(row("gone")).getByText("Not connected")).toBeInTheDocument()
  expect(screen.getByRole("navigation", { name: "Main" })).toHaveTextContent("Repositories")
})

test("people who administer no repository do not see the admin pages in the navigation", async () => {
  renderApp("/")
  const nav = await screen.findByRole("navigation", { name: "Main" }, slow)
  await screen.findByRole("button", { name: "Account" })
  expect(within(nav).queryByRole("link", { name: /Repositories/ })).not.toBeInTheDocument()
})

test("the wizard tests access, shows the branch protection to set and verifies it, then connects the repository", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  const { router } = renderApp("/admin/repositories/new")
  await user.type(await screen.findByRole("textbox", { name: /^Name/ }, slow), "network")
  await user.type(screen.getByRole("textbox", { name: /^Repository URL/ }), "http://gitea:3000/quai/network.git")
  await user.type(screen.getByRole("textbox", { name: "Credential reference" }), "env:READER")
  await user.click(screen.getByRole("button", { name: /Next/ }))

  // A reader's credential cannot push: the wizard says what it needs, and stops there.
  await user.click(screen.getByRole("button", { name: "Test access" }))
  const checks = await screen.findByRole("list", { name: "Access checks" })
  expect(within(checks).getByText(/Needs: write access to the repository/)).toBeInTheDocument()
  expect(within(checks).getAllByText(/Not run: the push check failed/)).toHaveLength(3)
  expect(screen.getByRole("button", { name: /Next/ })).toBeDisabled()

  // With the right credential, every check passes.
  await user.click(screen.getByRole("button", { name: /Back/ }))
  const credential = screen.getByRole("textbox", { name: "Credential reference" })
  await user.clear(credential)
  await user.type(credential, "env:QUAI_GITEA_TOKEN")
  await user.click(screen.getByRole("button", { name: /Next/ }))
  expect(screen.queryByRole("list", { name: "Access checks" })).not.toBeInTheDocument() // the old report is gone
  await user.click(screen.getByRole("button", { name: "Test access" }))
  expect(await screen.findByText(/3 types and 1 blueprints/)).toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Access checks" })).getAllByLabelText("Passed")).toHaveLength(8)
  await user.click(screen.getByRole("button", { name: /Next/ }))

  // Delivery and enforcement: the exact protection, verified on the platform.
  expect(screen.getByRole("list", { name: "Branch protection to set" })).toHaveTextContent("require quai/approval")
  await user.click(screen.getByRole("button", { name: "Verify the protection" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("branch main is not protected")
  await user.click(screen.getByRole("button", { name: /Next/ }))

  // The run token, shown once.
  await user.click(screen.getByRole("button", { name: "Generate a run token" }))
  expect(await screen.findByText("quai_run_0123456789abcdef")).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: /Next/ }))

  await user.type(screen.getByRole("textbox", { name: "Webhook secret reference" }), "env:HOOK_SECRET")
  await user.click(screen.getByRole("button", { name: /Next/ }))
  await user.click(screen.getByRole("button", { name: "Connect" }))
  expect(await screen.findByText("Connected")).toBeInTheDocument()
  const created = repos.sent.find((s) => s.method === "POST" && s.path === "repositories")?.body
  expect(created?.metadata.name).toBe("network")
  expect(created?.spec).toMatchObject({
    connector: "gitea",
    url: "http://gitea:3000/quai/network.git",
    credentials: { ref: "env:QUAI_GITEA_TOKEN" },
    webhooks: { secret: { ref: "env:HOOK_SECRET" } },
  })
  expect(created?.spec.runner).toBeUndefined() // no runner chosen: nothing empty is sent
  expect(created?.metadata.title).toBeUndefined()

  await user.click(screen.getByRole("button", { name: "Install the webhook" }))
  expect(await screen.findByText(/Installed: the platform delivers events to/)).toHaveTextContent("https://quai.example.com/webhooks/network")
  await user.click(screen.getByRole("link", { name: "Open its health" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/admin/repositories/network"))
})

test("the health page turns red when approvals are not enforced, and says what to fix", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  renderApp("/admin/repositories/infra-live")
  const alert = await screen.findByRole("alert", {}, slow)
  expect(alert).toHaveTextContent("Approvals are not enforced: Quai opens no pull request")
  expect(alert).toHaveTextContent("does not require the status check quai/approval")
  expect(screen.getByRole("list", { name: "Branch protection to set" })).toHaveTextContent("Enable status checks and require quai/approval")
  expect(screen.getByText(/within 30 days/)).toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Definition problems" })).getByText(".quai/types/dns.yaml")).toBeInTheDocument()
  expect(screen.getByText("opened", { exact: false })).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Test access" }))
  expect(within(await screen.findByRole("list", { name: "Access checks" })).getAllByLabelText("Passed")).toHaveLength(8)
})

test("a connection declared in configuration is read-only; one managed in Quai is saved and disconnected", async () => {
  state.user = "alice"
  const user = userEvent.setup()
  const { router } = renderApp("/admin/repositories/infra-live/edit")
  expect(await screen.findByText("Declared in Quai's configuration", {}, slow)).toBeInTheDocument()
  expect(screen.getByRole("textbox", { name: "Repository URL" })).toBeDisabled()
  expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument()

  await router.navigate({ to: "/admin/repositories/$name/edit", params: { name: "dns" } })
  const title = await screen.findByRole("textbox", { name: "Title" })
  expect(screen.getByRole("textbox", { name: /^Name/ })).toBeDisabled() // a name never changes
  await user.clear(title)
  await user.type(title, "Name servers")
  await user.click(screen.getByRole("button", { name: "Save" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/admin/repositories/dns"))
  expect(repos.sent.find((s) => s.method === "PUT")?.body?.metadata.title).toBe("Name servers")

  await router.navigate({ to: "/admin/repositories/$name/edit", params: { name: "dns" } })
  await user.click(await screen.findByRole("button", { name: "Disconnect" }))
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Disconnect" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/admin/repositories"))
  expect(repos.sent.some((s) => s.method === "DELETE" && s.path === "dns")).toBe(true)
})
