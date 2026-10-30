import { http, HttpResponse } from "msw"
import type { InstanceView, ResourceDetail, ResourceView } from "@/api/generated/types.gen"
import { sent, submitted, vm } from "./catalog"

// A fake inventory: VM files as the vm type reads them, one of them not matching its schema.

const run = { id: "r1", repository: "infra-live", stack: "envs/prod", commit: "abcdef1234", kind: "apply", status: "succeeded", startedAt: "2026-10-27T09:00:00Z" }

export const resources: ResourceView[] = [
  { id: "infra-live/vm/prod/web01", repository: "infra-live", type: "vm", key: "prod/web01", path: "envs/prod/vms/web01.yaml", params: { env: "prod", name: "web01" }, owner: "bob", stack: "envs/prod", status: "conforming", lastApply: run },
  { id: "infra-live/vm/dev/build-agent-01", repository: "infra-live", type: "vm", key: "dev/build-agent-01", path: "envs/dev/vms/build-agent-01.yaml", params: { env: "dev", name: "build-agent-01" }, owner: "alice", stack: "envs/dev", status: "conforming" },
  { id: "infra-live/vm/prod/legacy", repository: "infra-live", type: "vm", key: "prod/legacy", path: "envs/prod/vms/legacy.yaml", params: { env: "prod", name: "legacy" }, owner: "bob", stack: "envs/prod", status: "non-conforming", problems: { cpu: "must be at most 32" } },
]

const raw = "owner: bob\ntemplate: ubuntu-24.04\ncpu: 4          # bumped in INC-4411\nmemory_gb: 16\ndisks:\n  - size_gb: 80\n    tier: ssd\nnetwork:\n  vlan: 120\nbackup: true\n"

export function detail(r: ResourceView, user: string | null): ResourceDetail {
  return {
    ...r,
    commit: "abc123",
    raw,
    document: { owner: "bob", template: "ubuntu-24.04", cpu: 4, memory_gb: 16, disks: [{ size_gb: 80, tier: "ssd" }], network: { vlan: 120 }, backup: true },
    actions: [
      { name: "resize", title: "Resize", kind: "declarative", inputs: [
        { name: "cpu", title: "vCPUs", type: "integer", min: 1, max: 32, required: true },
        { name: "memory_gb", title: "Memory (GB)", type: "integer", min: 1, max: 256, required: true },
      ] },
      { name: "restart", title: "Restart", kind: "imperative", inputs: [] },
    ],
    history: [{ sha: "abc1234567", author: "Platform team", email: "p@example.com", date: "2026-10-20T08:00:00Z", subject: "the existing infrastructure" }],
    canOperate: user === r.owner || user === "alice",
    requests: [],
  }
}

export const shopDev: InstanceView = {
  repository: "infra-live",
  blueprint: "app-servers",
  blueprintVersion: "1.0.0",
  name: "shop-dev",
  owner: "bob",
  path: ".quai/instances/app-servers/shop-dev.yaml",
  inputs: { environment: "dev", app: "shop", webCpu: 2, withDatabase: true },
  resources: [
    { id: "db", resource: "infra-live/vm/dev/shop-db01", exists: false },
    { id: "web", resource: "infra-live/vm/dev/shop-web01", exists: true, status: "conforming" },
  ],
}

export function inventoryHandlers(user: () => string | null) {
  return [
    http.get("*/api/v1/instances", () => HttpResponse.json([shopDev])),
    http.get("*/api/v1/instances/infra-live/app-servers/shop-dev", () =>
      HttpResponse.json({ ...shopDev, canOperate: user() === "bob" || user() === "alice", requests: [{ id: submitted.id, number: 2, kind: "create", state: "applied", requester: "bob", createdAt: "2026-10-26T10:00:00Z" }] }),
    ),
    http.post("*/api/v1/plans/instance-action", async ({ request }) => {
      const b = (await request.json()) as { actionInputs?: { cpu?: number } }
      sent.plans.push(b)
      const cpu = b.actionInputs?.cpu
      if (typeof cpu === "number" && cpu > 16)
        return HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location: "body.actionInputs.cpu", message: "must be at most 16" }] }, { status: 422 })
      return HttpResponse.json({
        kind: "action",
        action: "scale-web",
        target: "app-servers/shop-dev",
        repository: "infra-live",
        blueprint: "app-servers",
        commit: "abc123",
        hash: "h4",
        changes: [{ path: "envs/dev/vms/shop-web01.yaml", op: "update", diff: [{ op: "-", text: "cpu: 2" }, { op: "+", text: `cpu: ${String(cpu)}` }] }],
        rules: [],
      })
    }),
    http.post("*/api/v1/requests/instance-action", async ({ request }) => {
      sent.requests.push((await request.json()) as Record<string, unknown>)
      return HttpResponse.json({ ...submitted, number: 9, id: "9c9e6679-7425-40de-944b-e07fc1f90ae9", kind: "action", action: "scale-web", target: "app-servers/shop-dev" }, { status: 201 })
    }),
    http.get("*/api/v1/inventory", () => HttpResponse.json(resources)),
    http.get("*/api/v1/resource", ({ request }) => {
      const r = resources.find((x) => x.id === new URL(request.url).searchParams.get("id"))
      return r ? HttpResponse.json(detail(r, user())) : HttpResponse.json({ status: 404, detail: "no resource" }, { status: 404 })
    }),
  ]
}

export { vm }
