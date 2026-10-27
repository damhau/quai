import type { ErrorModel } from "@/api/generated/types.gen"

// The API's errors are RFC 9457 problem details; field errors are located in the request body
// ("body.values.cpu", "body.params.env", "body.inputs.size"): docs/design/11-integrations.md.

export function isProblem(e: unknown): e is ErrorModel {
  return typeof e === "object" && e !== null && ("status" in e || "detail" in e)
}

/** fieldErrors returns the field errors located under prefix, by path: "body.values.cpu" → "cpu". */
export function fieldErrors(e: unknown, prefix: string): Record<string, string> {
  const out: Record<string, string> = {}
  if (!isProblem(e)) return out
  for (const d of e.errors ?? []) {
    const loc = d.location ?? ""
    if (loc === prefix) out.$ = d.message ?? "is invalid"
    else if (loc.startsWith(`${prefix}.`)) out[loc.slice(prefix.length + 1)] = d.message ?? "is invalid"
  }
  return out
}

export function hasFieldErrors(e: unknown): boolean {
  return isProblem(e) && (e.errors?.length ?? 0) > 0
}

/** problemMessage says what went wrong, for an error that is not about one field. */
export function problemMessage(e: unknown): string {
  if (isProblem(e)) return e.detail ?? e.title ?? "The request failed."
  if (e instanceof Error) return e.message
  return "The server cannot be reached."
}
