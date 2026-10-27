// @vitest-environment node
import { MaxExpressionSize, test as cel, toCel } from "@/lib/cel"

test("conditions from the example definitions", () => {
  expect(cel("has(inputs.backup) && inputs.backup", { inputs: { backup: true } })).toBe(true)
  expect(cel("has(inputs.backup) && inputs.backup", { inputs: {} })).toBe(false)
  expect(cel("inputs.withDatabase", { inputs: { withDatabase: false } })).toBe(false)
  expect(cel("inputs.size == 'large' && (!has(previous.size) || previous.size != 'large')", { inputs: { size: "large" } })).toBe(true)
  expect(cel("params.env == 'prod'", { params: { env: "prod" } })).toBe(true)
  expect(cel("'admins' in user.groups", { user: { name: "bob", groups: ["admins"] } })).toBe(true)
})

test("numbers follow the server: whole numbers are ints, and ints compare with doubles", () => {
  expect(toCel({ cpu: 4, ratio: 0.5, disks: [{ size_gb: 50 }] })).toEqual({ cpu: 4n, ratio: 0.5, disks: [{ size_gb: 50n }] })
  expect(cel("inputs.cpu > 8", { inputs: { cpu: 16 } })).toBe(true)
  expect(cel("inputs.cpu * 2 == 8", { inputs: { cpu: 4 } })).toBe(true)
  expect(cel("inputs.cpu > 2.5", { inputs: { cpu: 4 } })).toBe(true)
  expect(cel("inputs.disks.exists(d, d.size_gb >= 100)", { inputs: { disks: [{ size_gb: 50 }, { size_gb: 200 }] } })).toBe(true)
})

test("optional values", () => {
  expect(cel("inputs.?vlan.orValue(110) == 110", { inputs: {} })).toBe(true)
  expect(cel("inputs.?vlan.hasValue()", { inputs: { vlan: 120 } })).toBe(true)
})

test("what cannot be decided here is undefined: the form keeps the field and the server decides", () => {
  expect(cel("inputs.missing == 1", { inputs: {} })).toBeUndefined() // a missing key is an error, as in cel-go
  expect(cel("inputs.size +", {})).toBeUndefined() // does not parse
  expect(cel("nope == 1", {})).toBeUndefined() // unknown variable
  expect(cel("inputs.size", { inputs: { size: "large" } })).toBeUndefined() // not a bool
  expect(cel("'a'.format([])", {})).toBeUndefined() // not supported by the library
  expect(cel(`'${"a".repeat(MaxExpressionSize)}' == ''`, {})).toBeUndefined() // over the size limit
  expect(cel("", {})).toBe(true)
})
