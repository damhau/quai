// @vitest-environment node
import { UnsignedInt } from "@marcbachmann/cel-js/evaluator"
import { environment } from "@/lib/cel"
import cases from "./conformance.json"
import gaps from "./conformance-gaps.json"

// cel-go's conformance cases (exported by backend/tools/celcases: `npm run cel:cases`) run
// through the environment the forms use. The cases this library gets wrong are listed in
// conformance-gaps.json with the reason; the test fails when one of them starts passing, so the
// list stays exact across upgrades.

type Tagged = { t: string; v: unknown }
type Case = { file: string; section: string; name: string; expr: string; bindings?: Record<string, Tagged>; expect: Tagged | { error: true } }

const env = environment.clone({ unlistedVariablesAreDyn: true, enableOptionalTypes: true, homogeneousAggregateLiterals: false })

function input({ t, v }: Tagged): unknown {
  switch (t) {
    case "int":
      return BigInt(v as string)
    case "uint":
      return new UnsignedInt(BigInt(v as string))
    case "double":
      return typeof v === "string" ? Number(v) : v
    case "bytes":
      return Uint8Array.from(atob(v as string), (c) => c.charCodeAt(0))
    case "list":
      return (v as Tagged[]).map(input)
    case "map": {
      const entries = v as [Tagged, Tagged][]
      if (entries.every(([k]) => k.t === "string")) return Object.fromEntries(entries.map(([k, x]) => [k.v, input(x)]))
      return new Map(entries.map(([k, x]) => [input(k), input(x)]))
    }
    default:
      return v
  }
}

function tag(x: unknown): Tagged {
  if (x === null || x === undefined) return { t: "null", v: null }
  if (typeof x === "boolean") return { t: "bool", v: x }
  if (typeof x === "string") return { t: "string", v: x }
  if (typeof x === "bigint") return { t: "int", v: x.toString() }
  if (typeof x === "number") return { t: "double", v: x }
  if (x instanceof UnsignedInt) return { t: "uint", v: BigInt(x.valueOf()).toString() }
  if (x instanceof Uint8Array) return { t: "bytes", v: btoa(String.fromCharCode(...x)) }
  if (Array.isArray(x)) return { t: "list", v: x.map(tag) }
  if (x instanceof Map) return { t: "map", v: [...x.entries()].map(([k, e]) => [tag(k), tag(e)]) }
  if (typeof x === "object") return { t: "map", v: Object.entries(x).map(([k, e]) => [{ t: "string", v: k }, tag(e)]) }
  return { t: "other", v: String(x) }
}

function num(v: unknown) {
  return typeof v === "string" ? Number(v) : (v as number)
}

function equal(got: Tagged, want: Tagged): boolean {
  if (got.t !== want.t) return false
  switch (want.t) {
    case "double":
      return Number.isNaN(num(want.v)) ? Number.isNaN(got.v) : got.v === num(want.v)
    case "list": {
      const g = got.v as Tagged[]
      const w = want.v as Tagged[]
      return g.length === w.length && w.every((x, i) => equal(g[i]!, x))
    }
    case "map": {
      const g = got.v as [Tagged, Tagged][]
      const w = want.v as [Tagged, Tagged][]
      return g.length === w.length && w.every(([wk, wv]) => g.some(([gk, gv]) => equal(gk, wk) && equal(gv, wv)))
    }
    default:
      return got.v === want.v
  }
}

export function passes(c: Case): boolean {
  let got: Tagged | { error: true }
  try {
    const bindings = Object.fromEntries(Object.entries(c.bindings ?? {}).map(([k, v]) => [k, input(v)]))
    got = tag(env.evaluate(c.expr, bindings))
  } catch {
    got = { error: true }
  }
  if ("error" in c.expect) return "error" in got
  return !("error" in got) && equal(got, c.expect)
}

const id = (c: Case) => `${c.file}/${c.section}/${c.name}`
const known = new Map(Object.entries(gaps as Record<string, string[]>).flatMap(([reason, ids]) => ids.map((i) => [i, reason] as const)))

test("cel-go's conformance cases pass, except the known gaps", () => {
  const all = cases as Case[]
  const failing = all.filter((c) => !passes(c)).map(id)
  const unexpected = failing.filter((i) => !known.has(i))
  const fixed = [...known.keys()].filter((i) => !failing.includes(i))
  expect({ unexpected, fixed }).toEqual({ unexpected: [], fixed: [] })
  expect(all.length - failing.length).toBeGreaterThan(700)
})
