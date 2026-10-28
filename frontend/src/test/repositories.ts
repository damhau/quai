import { http, HttpResponse } from "msw"
import type { AccessView, CheckView, ConnectionView, Document, HealthView } from "@/api/generated/types.gen"

// A fake of the repository management API: the connections, what the wizard and the edit page
// send, and the health of infra-live.

const infraLive = (): ConnectionView => ({
  name: "infra-live",
  managedBy: "config",
  connected: true,
  document: {
    apiVersion: "quai.dev/v1beta1",
    kind: "Repository",
    metadata: { name: "infra-live", title: "Infrastructure" },
    spec: {
      connector: "gitea",
      url: "http://gitea:3000/quai/infra-live.git",
      defaultBranch: "main",
      credentials: { ref: "env:QUAI_GITEA_TOKEN" },
      delivery: { mode: "pull-request" },
    },
  },
})

const dns = (): ConnectionView => ({
  name: "dns",
  managedBy: "api",
  connected: true,
  document: {
    apiVersion: "quai.dev/v1beta1",
    kind: "Repository",
    metadata: { name: "dns", title: "DNS" },
    spec: { connector: "azure-devops", url: "https://dev.azure.com/acme/infra/_git/dns", defaultBranch: "main", credentials: { ref: "k8s:quai/dns-pat" } },
  },
})

const gone = (): ConnectionView => ({
  name: "gone",
  managedBy: "api",
  connected: false,
  error: "clone http://nowhere/gone.git: could not resolve host",
  document: { apiVersion: "quai.dev/v1beta1", kind: "Repository", metadata: { name: "gone" }, spec: { connector: "generic", url: "ssh://git@nowhere/gone.git" } },
})

export const repos: { connections: ConnectionView[]; sent: { method: string; path: string; body?: Document }[] } = { connections: [], sent: [] }

export function resetRepositories() {
  repos.connections = [infraLive(), dns(), gone()]
  repos.sent = []
}
resetRepositories()

export const health = (): HealthView => ({
  connected: true,
  credentials: { ref: "env:QUAI_GITEA_TOKEN", readable: true, expiresAt: "2026-11-10T00:00:00Z", expiresSoon: true },
  sync: { lastWebhookAt: "2026-10-28T10:00:00Z", lastFetchAt: "2026-10-28T10:00:00Z", lastPollAt: "2026-10-28T10:00:00Z", head: "4b825dc642cb6eb9a060e54bf8d69288fbee4904" },
  enforcement: { required: true, checked: true, problems: ["the protection of main does not require the status check quai/approval"], checkedAt: "2026-10-28T10:00:00Z" },
  trust: { checked: true, problems: [] },
  definitions: {
    commit: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
    types: 2,
    blueprints: 1,
    problems: [{ path: ".quai/types/dns.yaml", field: "spec.locate", message: "is required" }],
  },
  pullRequests: { open: 3, oldestOpenedAt: "2026-10-20T10:00:00Z", stale: 1 },
})

/** access answers the access test: a reader's credential cannot push. */
function access(d: Document): AccessView {
  const names: CheckView["name"][] = ["credentials", "connector", "read", "push", "pull-request", "status", "delete-branch", "branch-protection"]
  if (d.spec.credentials?.ref === "env:READER") {
    return {
      ok: false,
      checks: names.map((name) =>
        name === "push"
          ? { name, ok: false, error: "push: 403 Forbidden", permission: "write access to the repository, and no protection on the branches under quai/" }
          : ["pull-request", "status", "delete-branch"].includes(name)
            ? { name, ok: false, skipped: "the push check failed" }
            : { name, ok: true },
      ),
      scan: { commit: "4b825dc642cb6eb9a060e54bf8d69288fbee4904", found: false, types: 0, blueprints: 0, problems: [] },
    }
  }
  return {
    ok: true,
    checks: names.map((name) => ({ name, ok: true })),
    scan: { commit: "4b825dc642cb6eb9a060e54bf8d69288fbee4904", found: true, types: 3, blueprints: 1, problems: [] },
  }
}

const find = (name: unknown) => repos.connections.find((c) => c.name === name)
const notFound = () => HttpResponse.json({ status: 404, title: "Not Found", detail: "no such repository" }, { status: 404 })

export const repositoryHandlers = [
  http.get("*/api/v1/repository-connections", () => HttpResponse.json(repos.connections)),
  http.post("*/api/v1/repository-access-tests", async ({ request }) => HttpResponse.json(access((await request.json()) as Document))),
  http.post("*/api/v1/repository-enforcement-checks", async ({ request }) => {
    const d = (await request.json()) as Document
    repos.sent.push({ method: "POST", path: "enforcement", body: d })
    return HttpResponse.json({ required: true, checked: true, problems: ["branch main is not protected"], checkedAt: "2026-10-28T10:00:00Z" })
  }),
  http.post("*/api/v1/repositories", async ({ request }) => {
    const d = (await request.json()) as Document
    repos.sent.push({ method: "POST", path: "repositories", body: d })
    if (find(d.metadata.name)) return HttpResponse.json({ status: 409, title: "Conflict", detail: "a repository with this name is already connected" }, { status: 409 })
    const c: ConnectionView = { name: d.metadata.name, managedBy: "api", connected: true, document: d }
    repos.connections.push(c)
    return HttpResponse.json(c, { status: 201 })
  }),
  http.get("*/api/v1/repositories/:name/health", ({ params }) => (find(params.name) ? HttpResponse.json(health()) : notFound())),
  http.post("*/api/v1/repositories/:name/webhook", ({ params }) => HttpResponse.json({ url: `https://quai.example.com/webhooks/${String(params.name)}`, installed: true })),
  http.post("*/api/v1/repositories/:name/run-token", () =>
    HttpResponse.json({ token: "quai_run_0123456789abcdef", sha256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" }),
  ),
  http.get("*/api/v1/repositories/:name", ({ params }) => {
    const c = find(params.name)
    return c ? HttpResponse.json(c) : notFound()
  }),
  http.put("*/api/v1/repositories/:name", async ({ request, params }) => {
    const c = find(params.name)
    if (!c) return notFound()
    if (c.managedBy === "config") return HttpResponse.json({ status: 409, title: "Conflict", detail: "the repository is declared in Quai's configuration" }, { status: 409 })
    const d = (await request.json()) as Document
    repos.sent.push({ method: "PUT", path: String(params.name), body: d })
    c.document = d
    return HttpResponse.json(c)
  }),
  http.delete("*/api/v1/repositories/:name", ({ params }) => {
    repos.sent.push({ method: "DELETE", path: String(params.name) })
    repos.connections = repos.connections.filter((c) => c.name !== params.name)
    return new HttpResponse(null, { status: 204 })
  }),
]
