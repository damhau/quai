// The blueprint definition as the designer edits it: the file as written (quai.dev/v1beta1,
// docs/design/04-blueprints-and-change-sets.md), changed one part at a time so that what the
// designer has no editor for (actions, ui hints, extra schema keywords) is published unchanged.

export type Doc = Record<string, unknown>

export type Resource = {
  id: string
  type: { name: string; repository?: string; version?: string }
  params?: Record<string, unknown>
  values?: Record<string, unknown>
  when?: string
  dependsOn?: string[]
}

export type Rule = { name: string; when?: string; approvers: string[]; min?: number }

export type InputType = "string" | "integer" | "number" | "boolean"

/** Property is one field of the blueprint's form, as the inputs editor shows it. */
export type Property = {
  name: string
  type: InputType
  title: string
  required: boolean
  /** The default and the options, as typed: converted to the field's type when written. */
  default: string
  options: string
  /** The property's schema as written, for the keywords the editor does not show. */
  schema: Record<string, unknown>
}

const obj = (v: unknown): Record<string, unknown> => (v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {})

export function newBlueprint(): Doc {
  return {
    apiVersion: "quai.dev/v1beta1",
    kind: "Blueprint",
    metadata: { name: "", title: "", description: "", category: "", version: "1.0.0" },
    spec: { inputs: { type: "object", additionalProperties: false, properties: {} }, resources: [] },
  }
}

export const metadata = (d: Doc) => obj(d.metadata)
export const spec = (d: Doc) => obj(d.spec)

export function withMetadata(d: Doc, patch: Record<string, unknown>): Doc {
  return { ...d, metadata: { ...metadata(d), ...patch } }
}

function withSpec(d: Doc, patch: Record<string, unknown>): Doc {
  return { ...d, spec: { ...spec(d), ...patch } }
}

export function resources(d: Doc): Resource[] {
  const rs = spec(d).resources
  return Array.isArray(rs) ? (rs as Resource[]) : []
}

export function withResources(d: Doc, rs: Resource[]): Doc {
  return withSpec(d, { resources: rs })
}

/** renameResource changes a resource's id and the dependencies on it; false if the id is taken. */
export function renameResource(d: Doc, from: string, to: string): Doc | null {
  const rs = resources(d)
  if (!/^[a-z][a-z0-9-]{0,62}$/.test(to) || rs.some((r) => r.id === to)) return null
  return withResources(
    d,
    rs.map((r) => ({ ...r, id: r.id === from ? to : r.id, ...(r.dependsOn && { dependsOn: r.dependsOn.map((x) => (x === from ? to : x)) }) })),
  )
}

export function removeResource(d: Doc, id: string): Doc {
  return withResources(
    d,
    resources(d)
      .filter((r) => r.id !== id)
      .map((r) => (r.dependsOn?.includes(id) ? { ...r, dependsOn: r.dependsOn.filter((x) => x !== id) } : r)),
  )
}

/** addResource adds a resource of a type, named after it: vm, vm-2, … */
export function addResource(d: Doc, type: string): { doc: Doc; id: string } {
  const rs = resources(d)
  let id = type
  for (let n = 2; rs.some((r) => r.id === id); n++) id = `${type}-${n}`
  return { doc: withResources(d, [...rs, { id, type: { name: type }, params: {}, values: {} }]), id }
}

/** dependsOn makes `id` depend on `on`, unless that would make a cycle. */
export function addDependency(d: Doc, id: string, on: string): Doc | null {
  if (id === on || dependsTransitively(resources(d), on, id)) return null
  return withResources(
    d,
    resources(d).map((r) => (r.id === id && !r.dependsOn?.includes(on) ? { ...r, dependsOn: [...(r.dependsOn ?? []), on] } : r)),
  )
}

export function removeDependency(d: Doc, id: string, on: string): Doc {
  return withResources(
    d,
    resources(d).map((r) => (r.id === id ? { ...r, dependsOn: (r.dependsOn ?? []).filter((x) => x !== on) } : r)),
  )
}

function dependsTransitively(rs: Resource[], from: string, to: string, seen = new Set<string>()): boolean {
  if (from === to) return true
  if (seen.has(from)) return false
  seen.add(from)
  return (rs.find((r) => r.id === from)?.dependsOn ?? []).some((x) => dependsTransitively(rs, x, to, seen))
}

/** layout places resources in columns by dependency depth: what a resource depends on is left of it. */
export function layout(rs: Resource[]): Map<string, { x: number; y: number }> {
  const depth = new Map<string, number>()
  const of = (id: string, path: string[] = []): number => {
    if (depth.has(id)) return depth.get(id)!
    if (path.includes(id)) return 0
    const r = rs.find((x) => x.id === id)
    const d = Math.max(-1, ...(r?.dependsOn ?? []).filter((x) => rs.some((y) => y.id === x)).map((x) => of(x, [...path, id]))) + 1
    depth.set(id, d)
    return d
  }
  const rows = new Map<number, number>()
  const out = new Map<string, { x: number; y: number }>()
  for (const r of rs) {
    const col = of(r.id)
    const row = rows.get(col) ?? 0
    rows.set(col, row + 1)
    out.set(r.id, { x: col * 280, y: row * 140 })
  }
  return out
}

export function rules(d: Doc): Rule[] {
  const rs = obj(spec(d).approval).rules
  return Array.isArray(rs) ? (rs as Rule[]) : []
}

export function withRules(d: Doc, rs: Rule[]): Doc {
  return withSpec(d, { approval: { ...obj(spec(d).approval), rules: rs } })
}

const text = (v: unknown) => (v === undefined || v === null ? "" : String(v))

export function properties(d: Doc): Property[] {
  const inputs = obj(spec(d).inputs)
  const props = obj(inputs.properties)
  const required = Array.isArray(inputs.required) ? (inputs.required as string[]) : []
  const order = Array.isArray(obj(spec(d).ui).order) ? (obj(spec(d).ui).order as string[]) : []
  const names = [...order.filter((n) => n in props), ...Object.keys(props).filter((n) => !order.includes(n))]
  return names.map((name) => {
    const s = obj(props[name])
    const type = (["string", "integer", "number", "boolean"].includes(s.type as string) ? s.type : "string") as InputType
    return {
      name,
      type,
      title: text(s.title),
      required: required.includes(name),
      default: text(s.default),
      options: Array.isArray(s.enum) ? s.enum.map(text).join(", ") : "",
      schema: s,
    }
  })
}

/** typed converts text to a value of the field's type, or undefined when it is not one. */
export function typed(type: InputType, v: string): unknown {
  const t = v.trim()
  if (t === "") return undefined
  switch (type) {
    case "integer":
      return /^-?\d+$/.test(t) ? Number(t) : undefined
    case "number":
      return Number.isFinite(Number(t)) ? Number(t) : undefined
    case "boolean":
      return t === "true" ? true : t === "false" ? false : undefined
  }
  return t
}

export function withProperties(d: Doc, ps: Property[]): Doc {
  const inputs = obj(spec(d).inputs)
  const props: Record<string, unknown> = {}
  for (const p of ps) {
    const s: Record<string, unknown> = { ...p.schema, type: p.type, title: p.title || undefined }
    const options = p.options
      .split(",")
      .map((o) => typed(p.type, o))
      .filter((o) => o !== undefined)
    s.enum = options.length ? options : undefined
    s.default = typed(p.type, p.default)
    props[p.name] = Object.fromEntries(Object.entries(s).filter(([, v]) => v !== undefined))
  }
  const required = ps.filter((p) => p.required).map((p) => p.name)
  const next = { ...inputs, type: "object", properties: props, required: required.length ? required : undefined }
  return withSpec(d, {
    inputs: Object.fromEntries(Object.entries(next).filter(([, v]) => v !== undefined)),
    ui: { ...obj(spec(d).ui), order: ps.map((p) => p.name) },
  })
}
