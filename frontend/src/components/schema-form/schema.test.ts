// @vitest-environment node
import vm from "../../../../examples/gitops-repo/catalog/schemas/vm.schema.json"
import { defaults, deref, getPath, hiddenFields, localErrors, setPath, type JsonSchema } from "./schema"

const schema = vm as unknown as JsonSchema

test("defaults follow $refs and nested objects, as the server applies them", () => {
  expect(defaults(schema, schema)).toEqual({
    template: "ubuntu-24.04",
    cpu: 2,
    memory_gb: 4,
    disks: [{ size_gb: 50, tier: "standard" }],
    network: { vlan: 110 },
    backup: true,
    backup_policy: "daily",
  })
  expect(deref(schema.properties!.disks!.items, schema).properties?.size_gb?.maximum).toBe(4096)
})

test("paths set, read and remove values without changing the original", () => {
  const doc = { network: { vlan: 110 }, disks: [{ size_gb: 50 }] }
  const next = setPath(setPath(doc, "disks.1.size_gb", 100), "network.vlan", undefined)
  expect(next).toEqual({ network: {}, disks: [{ size_gb: 50 }, { size_gb: 100 }] })
  expect(doc).toEqual({ network: { vlan: 110 }, disks: [{ size_gb: 50 }] })
  expect(getPath(next, "disks.1.size_gb")).toBe(100)
})

test("visibleIf hides fields in path order, and keeps those it cannot evaluate", () => {
  const fields = { backup_policy: { visibleIf: "has(inputs.backup) && inputs.backup" }, z: { visibleIf: "inputs.nope" } }
  expect(hiddenFields(fields, { backup: false }, {})).toEqual(new Set(["backup_policy"]))
  expect(hiddenFields(fields, { backup: true }, {})).toEqual(new Set())
  // Each condition sees the fields hidden before it.
  expect(hiddenFields({ a: { visibleIf: "false" }, b: { visibleIf: "has(inputs.a)" } }, { a: 1 }, {})).toEqual(new Set(["a", "b"]))
})

test("local errors: required, ranges, patterns and items, skipping hidden fields", () => {
  const doc = { cpu: 64, memory_gb: 4, disks: [{ size_gb: 5 }], network: {}, description: "x".repeat(201) }
  expect(localErrors(schema, schema, doc, new Set(["description"]))).toEqual({
    template: "is required",
    cpu: "must be at most 32",
    "disks.0.size_gb": "must be at least 10",
    "network.vlan": "is required",
  })
})
