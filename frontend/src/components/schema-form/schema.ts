import { test, type Vars } from "@/lib/cel"

// JSON Schema helpers for the forms: the subset of draft 2020-12 that resource schemas use for
// form fields. The server validates with the full schema; these give instant feedback.

export type JsonSchema = {
  $ref?: string
  type?: string | string[]
  title?: string
  description?: string
  default?: unknown
  enum?: unknown[]
  const?: unknown
  format?: string
  pattern?: string
  minLength?: number
  maxLength?: number
  minimum?: number
  maximum?: number
  properties?: Record<string, JsonSchema>
  required?: string[]
  additionalProperties?: boolean | JsonSchema
  items?: JsonSchema
  minItems?: number
  maxItems?: number
  readOnly?: boolean
}

export type Values = Record<string, unknown>

/** deref follows a local $ref ("#/$defs/disk") against the root schema; other refs are left as they are. */
export function deref(s: JsonSchema | undefined, root: unknown): JsonSchema {
  let cur = s ?? {}
  for (let hops = 0; cur.$ref?.startsWith("#") && hops < 32; hops++) {
    let target: unknown = root
    for (const seg of cur.$ref.slice(1).split("/").filter(Boolean)) {
      const key = decodeURIComponent(seg).replaceAll("~1", "/").replaceAll("~0", "~")
      target = target !== null && typeof target === "object" ? (target as Record<string, unknown>)[key] : undefined
    }
    if (target === null || typeof target !== "object") break
    const rest = { ...cur }
    delete rest.$ref
    cur = { ...(target as JsonSchema), ...rest }
  }
  return cur
}

/** typeOf is the schema's main type: the first that is not null, or what its keywords imply. */
export function typeOf(s: JsonSchema): string | undefined {
  const t = Array.isArray(s.type) ? s.type.find((x) => x !== "null") : s.type
  if (t) return t
  if (s.enum?.length) return typeof s.enum[0] === "number" ? "number" : typeof s.enum[0]
  if (s.properties) return "object"
  return undefined
}

/** defaults returns the defaults a schema declares, nested objects included, as the server applies them. */
export function defaults(s: JsonSchema, root: unknown): unknown {
  const d = deref(s, root)
  if (d.default !== undefined) return structuredClone(d.default)
  if (typeOf(d) !== "object" || !d.properties) return undefined
  const out: Values = {}
  for (const [k, p] of Object.entries(d.properties)) {
    const v = defaults(p, root)
    if (v !== undefined) out[k] = v
  }
  return Object.keys(out).length ? out : undefined
}

export function join(base: string, k: string | number) {
  return base ? `${base}.${k}` : String(k)
}

export function getPath(doc: unknown, path: string): unknown {
  let cur = doc
  for (const seg of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined
    cur = (cur as Record<string, unknown>)[seg]
  }
  return cur
}

/** setPath returns a copy of doc with the value at path set, or removed when it is undefined. */
export function setPath(doc: Values, path: string, v: unknown): Values {
  const [head, ...rest] = path.split(".") as [string, ...string[]]
  const next: Values | unknown[] = Array.isArray(doc) ? [...(doc as unknown[])] : { ...doc }
  const cur = (next as Record<string, unknown>)[head]
  const value = rest.length ? setPath((cur !== null && typeof cur === "object" ? cur : /^\d+$/.test(rest[0]!) ? [] : {}) as Values, rest.join("."), v) : v
  if (value === undefined && !Array.isArray(next)) delete (next as Values)[head]
  else (next as Record<string, unknown>)[head] = value
  return next as Values
}

/**
 * hiddenFields returns the fields whose `visibleIf` is false, evaluated as the server does: in
 * path order, each with the values as pruned so far, as the variable `as` (`inputs` for a
 * document, `action` for an action's form). A condition the browser cannot evaluate keeps its
 * field: the server decides when it plans.
 */
export function hiddenFields(fields: Record<string, { visibleIf?: string }> | undefined, values: Values, vars: Vars, as: "inputs" | "action" = "inputs"): Set<string> {
  const hidden = new Set<string>()
  let pruned = values
  for (const path of Object.keys(fields ?? {}).sort()) {
    const cond = fields?.[path]?.visibleIf
    if (!cond) continue
    if (test(cond, { ...vars, [as]: pruned }) === false) {
      hidden.add(path)
      pruned = setPath(pruned, path, undefined)
    }
  }
  return hidden
}

/** constraintError checks a value against the basic constraints of its schema. */
export function constraintError(s: JsonSchema, v: unknown): string | undefined {
  if (v === undefined || v === null || v === "") return undefined
  if (typeof v === "number") {
    if (typeOf(s) === "integer" && !Number.isInteger(v)) return "must be a whole number"
    if (s.minimum !== undefined && v < s.minimum) return `must be at least ${s.minimum}`
    if (s.maximum !== undefined && v > s.maximum) return `must be at most ${s.maximum}`
  }
  if (typeof v === "string") {
    const n = [...v].length
    if (s.minLength !== undefined && n < s.minLength) return `must be at least ${s.minLength} characters`
    if (s.maxLength !== undefined && n > s.maxLength) return `must be at most ${s.maxLength} characters`
    if (s.pattern !== undefined) {
      try {
        if (!new RegExp(s.pattern, "u").test(v)) return `must match ${s.pattern}`
      } catch {
        return undefined // the server checks patterns JavaScript cannot compile
      }
    }
  }
  if (Array.isArray(v)) {
    if (s.minItems !== undefined && v.length < s.minItems) return `must have at least ${s.minItems} items`
    if (s.maxItems !== undefined && v.length > s.maxItems) return `must have at most ${s.maxItems} items`
  }
  return undefined
}

/** localErrors checks the visible fields of a document against required and the basic constraints. */
export function localErrors(schema: JsonSchema, root: unknown, value: unknown, hidden: Set<string>, path = "", out: Record<string, string> = {}) {
  const s = deref(schema, root)
  const t = typeOf(s)
  if (t === "object" && s.properties) {
    const obj = (value ?? {}) as Values
    for (const [k, p] of Object.entries(s.properties)) {
      const fp = join(path, k)
      if (hidden.has(fp)) continue
      const v = obj[k]
      if ((v === undefined || v === "") && s.required?.includes(k)) out[fp] = "is required"
      else localErrors(p, root, v, hidden, fp, out)
    }
    return out
  }
  const err = constraintError(s, value)
  if (err) out[path] = err
  if (t === "array" && Array.isArray(value) && s.items) value.forEach((x, i) => localErrors(s.items!, root, x, hidden, join(path, i), out))
  return out
}
