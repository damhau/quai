import type { Input, UiView } from "@/api/generated/types.gen"
import type { JsonSchema, Values } from "@/components/schema-form/schema"

// An action's inputs are a list of fields (definitions.Input), not a JSON Schema; the form
// renders them through the same schema form.

export function inputsSchema(inputs: Input[]): JsonSchema {
  return {
    type: "object",
    required: inputs.filter((i) => i.required).map((i) => i.name),
    properties: Object.fromEntries(
      inputs.map((i) => [
        i.name,
        {
          type: i.type === "enum" ? "string" : i.type,
          title: i.title ?? i.name,
          description: i.description,
          minimum: i.min,
          maximum: i.max,
          pattern: i.pattern,
          format: i.placeholder,
          enum: i.options?.length ? i.options : undefined,
          default: i.default,
        } satisfies JsonSchema,
      ]),
    ),
  }
}

export function inputsUi(inputs: Input[]): UiView {
  return { order: inputs.map((i) => i.name), fields: Object.fromEntries(inputs.filter((i) => i.visibleIf).map((i) => [i.name, { visibleIf: i.visibleIf }])) }
}

/**
 * initialInputs fills an action's form: the resource's current value of a field with the same
 * name (resize starts from the current size), else the input's default, as the server applies it.
 */
export function initialInputs(inputs: Input[], doc: Values | undefined): Values {
  const out: Values = {}
  for (const i of inputs) {
    const current = doc?.[i.name]
    const kind = i.type === "integer" ? "number" : i.type === "enum" ? "string" : i.type
    if (current !== undefined && typeof current === kind) out[i.name] = current
    else if (i.default !== undefined && i.default !== null) out[i.name] = i.default
    else if (i.type === "boolean") out[i.name] = false
    else if (i.type === "enum" && i.required && i.options?.length) out[i.name] = i.options[0]
  }
  return out
}
