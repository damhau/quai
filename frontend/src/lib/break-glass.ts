import type { BreakGlassView, MeOutputBody } from "@/api/generated/types.gen"

// Break-glass (docs/design/05-delivery-and-approvals.md#break-glass): what its review's pages say.

/** canReadBreakGlass says whether someone may list the break-glass reviews: auditors and platform admins. */
export function canReadBreakGlass(me: MeOutputBody | undefined): boolean {
  return (me?.roles ?? []).some((r) => (r.role === "auditor" || r.role === "platform-admin") && r.scope === "global")
}

const capacities: Record<string, string> = {
  "platform-admin": "a platform admin",
  auditor: "an auditor",
  owner: "an owner of the request",
}

/** capacityText is the capacity a sign-off was made in, as a sentence says it. */
export function capacityText(capacity: string): string {
  return capacities[capacity] ?? capacity
}

/** reviewStatus says where a break-glass review stands. */
export function reviewStatus(bg: BreakGlassView): string {
  if (!bg.open)
    return `Closed${bg.closedAt ? ` on ${new Date(bg.closedAt).toLocaleString([], { dateStyle: "medium", timeStyle: "short" })}` : ""}: signed off by ${(bg.signOffs ?? []).map((s) => s.signer).join(" and ")}.`
  const n = (bg.signOffs ?? []).length
  const left = bg.needed - n
  return `Open: ${n} of ${bg.needed} sign-offs; it waits for ${left} more, by platform admins, auditors or the request's owners other than the person who broke glass and the requester.`
}
