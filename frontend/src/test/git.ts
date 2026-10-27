import { http, HttpResponse } from "msw"
import type { BlueprintView } from "@/api/generated/types.gen"
import { appServers } from "./catalog"

// A fake backend for the Git log and the designer.

export const published: { blueprints: Record<string, unknown>[] } = { blueprints: [] }

const commits = [
  { sha: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", author: "bob", email: "bob@example.com", date: "2026-10-27T09:00:00Z", subject: "quai: create vm/prod/web01" },
  { sha: "0f1e2d3c4b5a69788796a5b4c3d2e1f001234567", author: "Platform team", email: "p@example.com", date: "2026-10-20T08:00:00Z", subject: "the existing infrastructure" },
]

export const appServersDefinition = {
  apiVersion: "quai.dev/v1beta1",
  kind: "Blueprint",
  metadata: { name: "app-servers", title: "Application servers", version: "1.0.0", category: "Applications" },
  spec: {
    inputs: appServers.schema,
    ui: { order: ["environment", "app", "webCpu", "withDatabase", "dbDiskGb"], fields: { dbDiskGb: { visibleIf: "inputs.withDatabase" } } },
    resources: [
      { id: "web", type: { name: "vm" }, params: { env: "${{ inputs.environment }}", name: "${{ inputs.app + '-web01' }}" }, values: { cpu: "${{ inputs.webCpu }}" }, dependsOn: ["db"] },
      { id: "db", when: "inputs.withDatabase", type: { name: "vm" }, params: { env: "${{ inputs.environment }}", name: "${{ inputs.app + '-db01' }}" }, values: { cpu: 4 } },
    ],
    approval: { rules: [{ name: "production", when: "inputs.environment == 'prod'", approvers: ["group:platform-admins"] }] },
    actions: [{ name: "scale-web", inputs: [{ name: "cpu", type: "integer" }], apply: { web: { action: "resize" } } }],
  },
}

export const gitHandlers = [
  http.get("*/api/v1/repositories", () =>
    HttpResponse.json([{ name: "infra-live", title: "Infrastructure", branch: "main", delivery: "direct", commit: commits[0]!.sha, types: 1, blueprints: 1, problems: [] }]),
  ),
  http.get("*/api/v1/repositories/infra-live/log", () => HttpResponse.json(commits)),
  http.get("*/api/v1/repositories/infra-live/commits/:sha", ({ params }) =>
    HttpResponse.json({
      sha: String(params.sha),
      text: `commit ${String(params.sha)}\nAuthor: bob <bob@example.com>\n\n    quai: create vm/prod/web01\n\ndiff --git a/envs/prod/vms/web01.yaml b/envs/prod/vms/web01.yaml\n@@ -0,0 +1 @@\n+cpu: 2\n`,
    }),
  ),
  http.get("*/api/v1/repositories/infra-live/blueprints/:name", ({ params }) => {
    const b = published.blueprints.findLast((x) => (x.metadata as { name?: string } | undefined)?.name === params.name)
    return b ? HttpResponse.json({ ...appServers, name: String(params.name), definition: b }) : undefined
  }),
  http.put("*/api/v1/repositories/infra-live/blueprints/:name", async ({ request, params }) => {
    const { blueprint } = (await request.json()) as { blueprint: Record<string, unknown> }
    published.blueprints.push(blueprint)
    const resources = (blueprint.spec as { resources?: unknown[] } | undefined)?.resources ?? []
    if (!resources.length)
      return HttpResponse.json({ status: 422, detail: "the blueprint does not load", errors: [{ location: "body.blueprint.spec.resources", message: "needs at least one resource" }] }, { status: 422 })
    const meta = blueprint.metadata as { name: string; title: string }
    const view: BlueprintView = { ...appServers, name: String(params.name), title: meta.title, definition: blueprint }
    return HttpResponse.json(view)
  }),
]
