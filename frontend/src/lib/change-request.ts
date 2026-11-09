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

/**
 * closeText says where closing the change request stands, once its content was applied or its
 * apply failed: Quai attaches the evidence, then closes it, unless it was never approved.
 */
export function closeText(c: ChangeRequestView): string {
  if (!c.closeOutcome) return ""
  const why = c.closeOutcome === "unsuccessful" ? "its apply failed" : "its content was applied"
  if (!c.closedAt) return `Quai is closing it, as ${why}: it attaches the evidence, then moves it to Closed.`
  switch (c.closeResult) {
    case "closed":
      return `Closed in ServiceNow by Quai, ${c.closeOutcome}: ${why}.`
    case "already":
      return `It was closed or canceled in ServiceNow already: Quai left it as it is, as ${why}.`
    case "left":
      return `Left to its change manager, as ${why}: it was never approved in ServiceNow, so Quai added a work note instead of closing it.`
    case "refused":
      return `Not closed: ServiceNow refused a step of closing it, as ${why}.`
  }
  return ""
}

/** sizeText is a file's size for people. */
export function sizeText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MiB`
}

/** closedEventText is the timeline's text of a change request Quai is done closing. */
export function closedEventText(changeRequest: string, outcome: string | undefined, result: string | undefined, files: number, problem: string | undefined): string {
  const evidence = files ? ", the evidence attached" : ""
  switch (result) {
    case "closed":
      return `closed change request ${changeRequest} in ServiceNow, ${outcome === "unsuccessful" ? "unsuccessful: the apply failed" : "successful"}${evidence}`
    case "already":
      return `change request ${changeRequest} was closed or canceled in ServiceNow already: Quai left it as it is${evidence}`
    case "left":
      return `left change request ${changeRequest} to its change manager: it was never approved in ServiceNow${evidence}`
  }
  return `could not close change request ${changeRequest}${evidence}${problem ? `: ${problem}` : ""}`
}

const outcomes: Record<string, string> = { rejected: "rejected", cancelled: "canceled", closed: "closed" }

/** outcomeVerb is what ServiceNow did to a change request that rejects its request. */
export const outcomeVerb = (outcome: string | undefined) => outcomes[outcome ?? ""] ?? "rejected"
