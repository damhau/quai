import type { ChangeRequestView } from "@/api/generated/types.gen"

// A request's change request in ServiceNow (docs/design/05-delivery-and-approvals.md#itsm): what
// its page says of it, from what Quai last read. Its link is the configured instance's form for
// the change request Quai created, never one an answer gave.

/** kindText names the kind of change. */
export const kindText = (c: ChangeRequestView) => (c.kind === "standard" ? "Standard change" : "Normal change")

/** readText says what ServiceNow reported last: its state and approval. */
export function readText(c: ChangeRequestView): string {
  if (!c.state) return ""
  return c.approval ? `${c.state}, approval ${c.approval}` : c.state
}

/**
 * outcomeText says what the change request means for the request, waiting or not on it (pending
 * approval or approved, not merging yet, without break-glass).
 */
export function outcomeText(c: ChangeRequestView, waiting: boolean): string {
  switch (c.outcome) {
    case "approved":
      return waiting ? "Approved in ServiceNow: the rule that waits on it is satisfied." : "Approved in ServiceNow."
    case "rejected":
      return "Rejected in ServiceNow: the request is rejected."
    case "cancelled":
      return "Canceled in ServiceNow: the request is rejected."
    case "closed":
      return "Closed in ServiceNow before the request merged: the request is rejected."
  }
  if (!waiting) return "Quai no longer waits on it."
  if (!c.number) return "Quai is opening it in ServiceNow."
  return "Waits for its approval in ServiceNow: Quai reads it every few minutes, and when this page opens."
}

/** supersededText says how many change requests of earlier content no longer count. */
export function supersededText(n: number | undefined): string {
  if (!n) return ""
  const which = n === 1 ? "The change request of earlier content no longer counts" : `The ${n} change requests of earlier content no longer count`
  return `${which}: an approval is for the content it was given for.`
}

const outcomes: Record<string, string> = { rejected: "rejected", cancelled: "canceled", closed: "closed" }

/** outcomeVerb is what ServiceNow did to a change request that rejects its request. */
export const outcomeVerb = (outcome: string | undefined) => outcomes[outcome ?? ""] ?? "rejected"
