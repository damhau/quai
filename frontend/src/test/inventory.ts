import { http, HttpResponse } from "msw"
import type { ResourceDetail, ResourceView } from "@/api/generated/types.gen"
import { vm } from "./catalog"

// A fake inventory: VM files as the vm type reads them, one of them not matching its schema.

const run = { id: "r1", repository: "infra-live", stack: "envs/prod", commit: "abcdef1234", kind: "apply", status: "succeeded", summary: null, startedAt: "2026-10-27T09:00:00Z" }

export const resources: ResourceView[] = [
  { id: "infra-live/vm/prod/web01", repository: "infra-live", type: "vm", key: "prod/web01", path: "envs/prod/vms/web01.yaml", params: { env: "prod", name: "web01" }, owner: "bob", stack: "envs/prod", status: "conforming", lastApply: run },
  { id: "infra-live/vm/dev/build-agent-01", repository: "infra-live", type: "vm", key: "dev/build-agent-01", path: "envs/dev/vms/build-agent-01.yaml", params: { env: "dev", name: "build-agent-01" }, owner: "alice", stack: "envs/dev", status: "conforming" },
  { id: "infra-live/vm/prod/legacy", repository: "infra-live", type: "vm", key: "prod/legacy", path: "envs/prod/vms/legacy.yaml", params: { env: "prod", name: "legacy" }, owner: "bob", stack: "envs/prod", status: "non-conforming", problems: { cpu: "must be at most 32" } },
]

const raw = "owner: bob\ntemplate: ubuntu-24.04\ncpu: 4          # bumped in INC-4411\nmemory_gb: 16\ndisks:\n  - size_gb: 80\n    tier: ssd\nnetwork:\n  vlan: 120\nbackup: true\n"

function detail(r: ResourceView, user: string | null): ResourceDetail {
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

export function inventoryHandlers(user: () => string | null) {
  return [
    http.get("*/api/v1/inventory", () => HttpResponse.json(resources)),
    http.get("*/api/v1/resource", ({ request }) => {
      const r = resources.find((x) => x.id === new URL(request.url).searchParams.get("id"))
      return r ? HttpResponse.json(detail(r, user())) : HttpResponse.json({ status: 404, detail: "no resource" }, { status: 404 })
    }),
  ]
}

export { vm }
