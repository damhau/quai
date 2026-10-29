import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { RunView } from "@/api/generated/types.gen"
import { resources } from "@/test/inventory"
import { renderApp } from "@/test/render"
import { server, state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk

const run = (kind: string, status: string, stack: string, commit: string, extra: Partial<RunView> = {}): RunView => ({
  id: `${stack}-${kind}-${commit}`,
  repository: "infra-live",
  stack,
  commit,
  kind,
  status,
  startedAt: "2026-10-28T09:00:00Z",
  finishedAt: "2026-10-28T09:05:00Z",
  ...extra,
})

test("the inventory shows what each resource's runs say since its last change", async () => {
  server.use(
    http.get("*/api/v1/inventory", () =>
      HttpResponse.json([
        { ...resources[0], state: "drifted", stateRun: run("drift-check", "succeeded", "envs/prod", "abcdef1") },
        { ...resources[1], state: "in_repo" },
        { ...resources[2], state: "failed", stateRun: run("apply", "failed", "envs/prod", "abcdef1") },
      ]),
    ),
  )
  renderApp("/inventory")
  const web = (await screen.findByRole("link", { name: "prod/web01" }, slow)).closest("tr") as HTMLElement
  expect(within(web).getByText("Drifted")).toBeInTheDocument()
  expect(within(web).getByText("Conforming")).toBeInTheDocument()
  expect(within(screen.getByRole("link", { name: "dev/build-agent-01" }).closest("tr") as HTMLElement).getByText("In repo")).toBeInTheDocument()
  expect(within(screen.getByRole("link", { name: "prod/legacy" }).closest("tr") as HTMLElement).getByText("Failed")).toBeInTheDocument()
  expect(screen.getByRole("columnheader", { name: "Status" })).toBeInTheDocument()
})

test("a resource's page says its status and the run behind it, and which commits Quai made", async () => {
  state.user = "bob"
  server.use(
    http.get("*/api/v1/resource", () =>
      HttpResponse.json({
        ...resources[0],
        state: "failed",
        stateRun: run("apply", "failed", "envs/prod", "abcdef1234"),
        commit: "abc123",
        raw: "cpu: 4\n",
        actions: [],
        canOperate: false,
        requests: [],
        history: [
          { sha: "1111111111", author: "bob", email: "bob@example.com", date: "2026-10-28T08:00:00Z", subject: "Resize prod/web01", request: 7 },
          { sha: "2222222222", author: "Dana", email: "dana@example.com", date: "2026-10-20T08:00:00Z", subject: "tune by hand" },
        ],
      }),
    ),
  )
  renderApp("/resources/infra-live/vm/prod/web01")
  expect(await screen.findByText("Failed", {}, slow)).toBeInTheDocument()
  expect(screen.getByText(/apply of abcdef1,/)).toBeInTheDocument()
  expect(screen.getByText("Quai #7")).toBeInTheDocument()
  expect(screen.getAllByText(/Quai #/)).toHaveLength(1)
})

test("a repository's page lists its runs by stack", async () => {
  state.user = "alice"
  server.use(
    http.get("*/api/v1/repositories/:name/runs", () =>
      HttpResponse.json([
        run("apply", "succeeded", "envs/prod", "abcdef1", { url: "https://ci.example.com/3" }),
        run("plan", "running", "envs/prod", "fedcba9", { pullRequest: "12" }),
        run("apply", "failed", "envs/dev", "1234567"),
      ]),
    ),
  )
  renderApp("/admin/repositories/infra-live")
  const prod = await screen.findByRole("region", { name: "Runs of envs/prod" }, slow)
  expect(within(prod).getAllByRole("listitem")).toHaveLength(2)
  expect(within(prod).getByText("pull request #12")).toBeInTheDocument()
  expect(within(prod).getByRole("link", { name: "run" })).toHaveAttribute("href", "https://ci.example.com/3")
  expect(within(screen.getByRole("region", { name: "Runs of envs/dev" })).getByText("Failed")).toBeInTheDocument()
})

test("with a drift-check pipeline, a repository's page queues the drift checks", async () => {
  state.user = "alice"
  let queued = false
  server.use(
    http.get("*/api/v1/repositories/:name", () =>
      HttpResponse.json({
        name: "infra-live",
        managedBy: "api",
        connected: true,
        document: {
          apiVersion: "quai.dev/v1beta1",
          kind: "Repository",
          metadata: { name: "infra-live" },
          spec: { connector: "azure-devops", url: "https://dev.azure.com/acme/infra/_git/live", runner: { pipelines: [{ name: "infra-drift", kind: "drift-check", stacks: "envs/{env}" }] } },
        },
      }),
    ),
    http.get("*/api/v1/repositories/:name/runs", () => HttpResponse.json(queued ? [run("drift-check", "running", "envs/prod", "abcdef1")] : [])),
    http.post("*/api/v1/repositories/:name/drift-checks", () => {
      queued = true
      return HttpResponse.json([run("drift-check", "running", "envs/prod", "abcdef1")], { status: 202 })
    }),
  )
  const user = (await import("@testing-library/user-event")).default.setup()
  renderApp("/admin/repositories/infra-live")
  await user.click(await screen.findByRole("button", { name: "Check drift" }, slow))
  expect(await screen.findByText("Drift checks queued: envs/prod.")).toBeInTheDocument()
  expect(await screen.findByRole("region", { name: "Runs of envs/prod" })).toHaveTextContent("drift-check")
})

test("a drifted resource says what differs", async () => {
  state.user = "bob"
  server.use(
    http.get("*/api/v1/resource", () =>
      HttpResponse.json({
        ...resources[0],
        state: "drifted",
        stateRun: run("drift-check", "succeeded", "envs/prod", "abcdef1", {
          plan: { resources: [{ resource: "infra-live/vm/prod/web01", type: "vm", key: "prod/web01", changes: [{ address: 'module.vm["web01"].terraform_data.vm', action: "update", attributes: ["input.cpu"] }] }] },
        }),
        commit: "abc123",
        raw: "cpu: 4\n",
        actions: [],
        canOperate: false,
        requests: [],
        history: [],
      }),
    ),
  )
  renderApp("/resources/infra-live/vm/prod/web01")
  const differs = await screen.findByRole("list", { name: "What differs" }, slow)
  expect(differs).toHaveTextContent('module.vm["web01"].terraform_data.vm update: input.cpu')
  expect(screen.getByText("Drifted")).toBeInTheDocument()
})
