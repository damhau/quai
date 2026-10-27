import { http, HttpResponse } from "msw"
import type { BlueprintView, PlanView, RequestView, TypeView } from "@/api/generated/types.gen"
import vmSchema from "../../../examples/gitops-repo/catalog/schemas/vm.schema.json"

// A fake backend for the catalog and requests, shaped after examples/gitops-repo: the vm type and
// the app-servers blueprint. Plans are made up from the request, enough for the pages to show
// changes, approvals and field errors.

export const vm: TypeView = {
  kind: "type",
  repository: "infra-live",
  name: "vm",
  title: "Virtual machine (Terraform)",
  description: "One YAML file per VM under envs/<env>/vms/.",
  category: "Compute",
  version: "1.0.0",
  tags: ["terraform"],
  path: ".quai/types/vm.yaml",
  locate: "envs/{env}/vms/{name}.yaml",
  params: [
    { name: "env", title: "Environment", type: "enum", options: ["dev", "prod", "test"] },
    { name: "name", title: "VM name", type: "string", pattern: "[a-z][a-z0-9-]{1,30}", description: "Becomes the file name." },
  ],
  schema: vmSchema,
  propertyOrder: ["owner", "description", "template", "cpu", "memory_gb", "disks", "network", "backup", "backup_policy", "tags", "vlan", "ip", "size_gb", "tier"],
  ui: {
    order: ["description", "template", "cpu", "memory_gb", "disks", "network", "backup", "backup_policy", "tags"],
    fields: { template: { immutable: true }, "network.vlan": { immutable: true }, backup_policy: { visibleIf: "has(inputs.backup) && inputs.backup" } },
  },
  actions: [],
  stack: "envs/{env}",
  deletion: "approval",
  ownerField: "owner",
  managedFields: ["owner"],
}

export const appServers: BlueprintView = {
  kind: "blueprint",
  repository: "infra-live",
  name: "app-servers",
  title: "Application servers",
  description: "A web server and its database server.",
  category: "Applications",
  version: "1.0.0",
  tags: ["vm"],
  path: "blueprints/app-servers.yaml",
  schema: {
    type: "object",
    required: ["environment", "app"],
    properties: {
      environment: { type: "string", title: "Environment", enum: ["dev", "test", "prod"], default: "dev" },
      app: { type: "string", title: "Application", pattern: "^[a-z][a-z0-9]{1,20}$" },
      webCpu: { type: "integer", title: "Web server vCPUs", minimum: 1, maximum: 16, default: 2 },
      withDatabase: { type: "boolean", title: "Add a database server", default: true },
      dbDiskGb: { type: "integer", title: "Database data disk (GB)", minimum: 50, maximum: 4096, default: 200 },
    },
  },
  propertyOrder: ["environment", "app", "webCpu", "withDatabase", "dbDiskGb"],
  ui: { order: ["environment", "app", "webCpu", "withDatabase", "dbDiskGb"], fields: { dbDiskGb: { visibleIf: "inputs.withDatabase" } } },
  resources: [
    { id: "web", repository: "infra-live", type: "vm", dependsOn: ["db"] },
    { id: "db", repository: "infra-live", type: "vm", when: "inputs.withDatabase", dependsOn: [] },
  ],
  actions: [{ name: "scale-web", title: "Scale the web server", kind: "declarative", inputs: [{ name: "cpu", title: "vCPUs", type: "integer", min: 1, max: 16, required: true }] }],
}

type Body = Record<string, unknown> & { values?: Record<string, unknown>; params?: Record<string, string>; inputs?: Record<string, unknown> }

/** What the pages sent, for the tests to check. */
export const sent: { plans: Body[]; requests: Body[] } = { plans: [], requests: [] }

const yaml = (doc: Record<string, unknown>) => Object.entries(doc).map(([k, v]) => ({ op: "+" as const, text: `${k}: ${JSON.stringify(v)}` }))

function day2Plan(b: Body): PlanView | Response {
  const key = String(b.key)
  const values = b.kind === "update" ? (b.values ?? {}) : (b.actionInputs as Record<string, unknown> | undefined) ?? {}
  const prefix = b.kind === "update" ? "body.values" : "body.actionInputs"
  if (typeof values.cpu === "number" && values.cpu > 32)
    return HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location: `${prefix}.cpu`, message: "must be at most 32" }] }, { status: 422 })
  const diff =
    b.kind === "delete"
      ? [{ op: "-" as const, text: "cpu: 4" }]
      : [
          { op: "-" as const, text: "cpu: 4          # bumped in INC-4411" },
          { op: "+" as const, text: `cpu: ${String(values.cpu)}          # bumped in INC-4411` },
        ]
  return {
    kind: String(b.kind),
    action: b.action as string | undefined,
    target: `infra-live/vm/${key}`,
    repository: "infra-live",
    type: "vm",
    commit: "abc123",
    hash: "h3",
    changes: [{ path: `envs/${key.split("/")[0]}/vms/${key.split("/")[1]}.yaml`, op: b.kind === "delete" ? "delete" : "update", diff }],
    rules: key.startsWith("prod/") ? [{ names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: [], satisfied: false }] : [],
  }
}

function planOf(b: Body): PlanView | Response {
  if (b.type === "vm" && b.kind !== "create") return day2Plan(b)
  if (b.type === "vm") {
    const cpu = b.values?.cpu
    if (typeof cpu === "number" && cpu > 32)
      return HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location: "body.values.cpu", message: "must be at most 32" }] }, { status: 422 })
    const { env, name } = b.params ?? {}
    return {
      kind: "create",
      target: `infra-live/vm/${env}/${name}`,
      repository: "infra-live",
      type: "vm",
      stack: `envs/${env}`,
      commit: "abc123",
      hash: "h1",
      changes: [{ path: `envs/${env}/vms/${name}.yaml`, op: "create", diff: yaml({ owner: "bob", ...b.values }) }],
      rules: env === "prod" ? [{ names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: [], satisfied: false }] : [],
    }
  }
  if (b.name === "taken") return HttpResponse.json({ status: 409, detail: "instance app-servers/taken: .quai/instances/app-servers/taken.yaml already exists" }, { status: 409 })
  const inputs = b.inputs ?? {}
  const files = [`envs/${String(inputs.environment)}/vms/${String(inputs.app)}-web01.yaml`]
  if (inputs.withDatabase) files.push(`envs/${String(inputs.environment)}/vms/${String(inputs.app)}-db01.yaml`)
  files.push(`.quai/instances/app-servers/${String(b.name)}.yaml`)
  return {
    kind: "create",
    target: `infra-live/app-servers/${String(b.name)}`,
    repository: "infra-live",
    blueprint: "app-servers",
    commit: "abc123",
    hash: "h2",
    changes: files.map((path) => ({ path, op: "create" as const, diff: [{ op: "+" as const, text: "apiVersion: quai.dev/v1beta1" }] })),
    rules: [],
  }
}

export const submitted: RequestView = {
  id: "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  number: 7,
  kind: "create",
  target: "infra-live/vm/prod/web01",
  repository: "infra-live",
  type: "vm",
  requester: "bob",
  reason: "Needed for the launch",
  state: "pending",
  inputs: {},
  createdAt: "2026-10-27T10:00:00Z",
  updatedAt: "2026-10-27T10:00:00Z",
  canReview: false,
  canCancel: true,
  canRetry: false,
  rules: [{ names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: [], satisfied: false }],
  changes: [{ path: "envs/prod/vms/web01.yaml", op: "create", diff: [{ op: "+", text: "cpu: 2" }] }],
}

const respond = (p: PlanView | Response) => (p instanceof Response ? p : HttpResponse.json(p))

export const catalogHandlers = [
  http.get("*/api/v1/catalog", () =>
    HttpResponse.json([
      { kind: "type", repository: "infra-live", name: "vm", title: vm.title, description: vm.description, category: "Compute", tags: ["terraform"] },
      { kind: "blueprint", repository: "infra-live", name: "app-servers", title: appServers.title, description: appServers.description, category: "Applications", tags: ["vm"] },
    ]),
  ),
  http.get("*/api/v1/repositories/infra-live/types/vm", () => HttpResponse.json(vm)),
  http.get("*/api/v1/repositories/infra-live/blueprints/app-servers", async () => HttpResponse.json({ ...appServers, definition: (await import("./git")).appServersDefinition })),
  http.post("*/api/v1/plans/type", async ({ request }) => {
    const b = (await request.json()) as Body
    sent.plans.push(b)
    return respond(planOf(b))
  }),
  http.post("*/api/v1/plans/blueprint", async ({ request }) => {
    const b = (await request.json()) as Body
    sent.plans.push(b)
    return respond(planOf(b))
  }),
  http.post("*/api/v1/requests/type", async ({ request }) => {
    sent.requests.push((await request.json()) as Body)
    return HttpResponse.json(submitted, { status: 201 })
  }),
  http.post("*/api/v1/requests/blueprint", async ({ request }) => {
    sent.requests.push((await request.json()) as Body)
    return HttpResponse.json({ ...submitted, number: 8, id: "8c9e6679-7425-40de-944b-e07fc1f90ae8" }, { status: 201 })
  }),
  // The requests just submitted; other requests are in ./requests.
  http.get("*/api/v1/requests/:id", ({ params }) => (/^[789]c9e/.test(String(params.id)) ? HttpResponse.json({ ...submitted, id: String(params.id) }) : undefined)),
]
