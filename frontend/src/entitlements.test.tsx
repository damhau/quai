import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { http, HttpResponse } from "msw"
import type { CatalogItem, EntitlementsView } from "@/api/generated/types.gen"
import { appServers, vm } from "@/test/catalog"
import { renderApp } from "@/test/render"
import { server } from "@/test/server"

// The server decides who sees and requests what (docs/design/08, entitlements): it leaves hidden
// items out and refuses their plans. These pages say what it decided.

const prodClosed: EntitlementsView = {
  visibility: ["group:developers", "group:finops"],
  requesters: ["group:developers"],
  environments: [{ environment: "prod", requesters: ["group:shop"], canRequest: false }],
  canRequest: true,
}

const item = (name: string, title: string, entitlements: EntitlementsView): CatalogItem => ({
  kind: "type",
  repository: "infra-live",
  name,
  title,
  category: "Data",
  tags: [],
  entitlements,
})

test("the catalog marks restricted items and says what you cannot request", async () => {
  server.use(
    http.get("*/api/v1/catalog", () =>
      HttpResponse.json([
        item("database", "Database", prodClosed),
        item("warehouse", "Warehouse", { visibility: [], requesters: ["group:data-*"], environments: [], canRequest: false, reason: "this catalog item is not open to your groups" }),
      ]),
    ),
  )
  renderApp("/")
  expect(await screen.findByRole("link", { name: "Request Database" })).toBeInTheDocument()
  expect(screen.getByText("Not open to your groups in prod")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Request Warehouse" })).toBeDisabled()
  expect(screen.queryByRole("link", { name: "Request Warehouse" })).not.toBeInTheDocument()
  expect(screen.getByText("You cannot request this: this catalog item is not open to your groups")).toBeInTheDocument()
  const badges = screen.getAllByText("Restricted")
  expect(badges).toHaveLength(2)
  expect(badges[0]!.closest("[title]")).toHaveAttribute("title", "Visible to group:developers, group:finops\nRequested by group:developers\nIn prod, only group:shop")
})

test("a request form says who may request it, and warns when its environment is closed to you", async () => {
  server.use(http.get("*/api/v1/repositories/infra-live/types/vm", () => HttpResponse.json({ ...vm, entitlements: prodClosed })))
  const user = userEvent.setup()
  renderApp("/request/infra-live/types/vm")
  const note = await screen.findByRole("note", { name: "Who may request this item" })
  expect(within(note).getByText("Restricted item")).toBeInTheDocument()
  expect(within(note).getByText("In prod, only group:shop")).toBeInTheDocument()

  await user.click(screen.getByRole("radio", { name: "prod" }))
  const alert = screen.getByRole("alert", { name: "Who may request this item" })
  expect(within(alert).getByText("You cannot request this: this catalog item is not open to your groups in prod")).toBeInTheDocument()
  await user.click(screen.getByRole("radio", { name: "dev" }))
  expect(screen.getByRole("note", { name: "Who may request this item" })).toBeInTheDocument()
})

test("an unrestricted item's form says nothing about who may request it", async () => {
  renderApp("/request/infra-live/types/vm")
  expect(await screen.findByRole("heading", { name: "Virtual machine (Terraform)" })).toBeInTheDocument()
  expect(screen.queryByRole("note", { name: "Who may request this item" })).not.toBeInTheDocument()
})

test("a preview lists who each item would let see and request it", async () => {
  server.use(
    http.get("*/api/v1/repositories/infra-live/preview", () =>
      HttpResponse.json({
        repository: "infra-live",
        branch: "restrict",
        commit: "2a723df34bd8897cbad09682ec0d6cb63b684146",
        items: [item("database", "Database", { visibility: ["group:dba"], requesters: [], environments: [{ environment: "prod", requesters: ["group:shop"] }] })],
        problems: [],
      }),
    ),
  )
  renderApp("/preview/infra-live?branch=restrict")
  const lines = await screen.findByRole("list", { name: "Who may request Database" })
  expect(within(lines).getByText("Visible to group:dba")).toBeInTheDocument()
  expect(within(lines).getByText("Requested by everyone")).toBeInTheDocument()
  expect(within(lines).getByText("In prod, only group:shop")).toBeInTheDocument()
})

test("a resource's page and a deployment's describe their item for them, whatever its entitlements", async () => {
  const asked: string[] = []
  server.use(
    http.get("*/api/v1/repositories/infra-live/types/vm", ({ request }) => {
      asked.push(new URL(request.url).search)
      return HttpResponse.json(vm)
    }),
    http.get("*/api/v1/repositories/infra-live/blueprints/app-servers", ({ request }) => {
      asked.push(new URL(request.url).search)
      return HttpResponse.json(appServers)
    }),
  )
  renderApp("/resources/infra-live/vm/prod/web01")
  expect(await screen.findByRole("heading", { name: "web01" })).toBeInTheDocument()
  expect(asked).toContain("?resource=prod%2Fweb01")
  renderApp("/deployments/infra-live/app-servers/shop-dev")
  expect(await screen.findByRole("heading", { name: /shop-dev/ })).toBeInTheDocument()
  expect(asked).toContain("?instance=shop-dev")
})
