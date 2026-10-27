import { Environment, type ParseResult } from "@marcbachmann/cel-js"

// Form conditions (`visibleIf`) evaluated in the browser, for instant feedback, with the variables
// and options of the backend's environment (backend/internal/engine/expr): optional values, and
// lists and maps that may mix types. The server's evaluation is authoritative. When this one
// cannot answer (an expression it does not support, a key that is missing), test returns
// undefined and the form keeps the field visible; the plan then reports the server's answer.
// The library was chosen against cel-go's conformance cases: see conformance.test.ts and
// docs/engineering/frontend.md.

/** MaxExpressionSize is the backend's limit on an expression, in code points. */
export const MaxExpressionSize = 4096

export const environment = new Environment({ enableOptionalTypes: true, homogeneousAggregateLiterals: false })
  .registerVariable("inputs", "map<string, dyn>")
  .registerVariable("params", "map<string, dyn>")
  .registerVariable("doc", "dyn")
  .registerVariable("previous", "map<string, dyn>")
  .registerVariable("action", "map<string, dyn>")
  .registerVariable("request", "map<string, string>")
  .registerVariable("user", "map<string, dyn>")

export type Vars = Partial<Record<"inputs" | "params" | "doc" | "previous" | "action" | "request" | "user", unknown>>

const programs = new Map<string, ParseResult | null>()

function compile(src: string): ParseResult | null {
  let p = programs.get(src)
  if (p === undefined) {
    try {
      p = [...src].length <= MaxExpressionSize ? environment.parse(src) : null
      if (p && !p.check().valid) p = null
    } catch {
      p = null
    }
    programs.set(src, p)
  }
  return p
}

/** toCel converts a value of the JSON data model to CEL's: whole numbers are ints, as on the server. */
export function toCel(v: unknown): unknown {
  if (typeof v === "number") return Number.isInteger(v) ? BigInt(v) : v
  if (Array.isArray(v)) return v.map(toCel)
  if (v !== null && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, toCel(x)]))
  return v
}

/** test evaluates a condition: true, false, or undefined when it cannot be evaluated here. */
export function test(src: string, vars: Vars): boolean | undefined {
  if (src.trim() === "") return true
  const p = compile(src)
  if (!p) return undefined
  const ctx = { inputs: {}, params: {}, doc: {}, previous: {}, action: {}, request: {}, user: {}, ...vars }
  try {
    const out: unknown = p(toCel(ctx) as Record<string, unknown>)
    return typeof out === "boolean" ? out : undefined
  } catch {
    return undefined
  }
}
