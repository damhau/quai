import type { DiffLine } from "@/api/generated/types.gen"

/** fold keeps the lines around changes, replacing longer unchanged runs with null. */
export function fold(lines: DiffLine[], context: number): (DiffLine | null)[] {
  const near = lines.map((_, i) => lines.slice(Math.max(0, i - context), i + context + 1).some((l) => l.op !== " "))
  const out: (DiffLine | null)[] = []
  lines.forEach((l, i) => {
    if (near[i]) out.push(l)
    else if (out.at(-1) !== null) out.push(null)
  })
  return out
}
