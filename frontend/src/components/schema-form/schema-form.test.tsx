import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useState } from "react"
import { SchemaForm } from "./schema-form"
import type { Values } from "./schema"

const schema = {
  type: "object",
  properties: { values: { type: "array", title: "Values", items: { type: "string" }, minItems: 1 } },
}

function Form({ initial, seen }: { initial: Values; seen: (v: Values) => void }) {
  const [value, setValue] = useState(initial)
  return (
    <SchemaForm
      schema={schema}
      value={value}
      onChange={(v) => {
        setValue(v)
        seen(v)
      }}
    />
  )
}

test("each item of a list of values is named after the list and its place", async () => {
  const user = userEvent.setup()
  const seen = vi.fn()
  render(<Form initial={{ values: ["10.0.13.30", "10.0.13.31"] }} seen={seen} />)
  const second = screen.getByRole("textbox", { name: "Values 2" })
  expect(screen.getByRole("textbox", { name: "Values 1" })).toHaveValue("10.0.13.30")
  expect(second).toHaveValue("10.0.13.31")
  await user.clear(second)
  await user.type(second, "10.0.13.32")
  expect(seen).toHaveBeenLastCalledWith({ values: ["10.0.13.30", "10.0.13.32"] })
})
