import type { RequestView, ReviewForView } from "@/api/generated/types.gen"

// Delegation (docs/design/05-delivery-and-approvals.md#delegation): an approver hands their
// approvals to a colleague for a period. The profile page makes, lists and revokes one's own; the
// request page lets a delegate review in their own right or on a delegator's behalf, and names
// both people wherever a review made on someone's behalf appears.

/**
 * localInput is an instant as a datetime-local input shows it: in the browser's time zone, to
 * the minute.
 */
export function localInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/**
 * instantOf is the instant a datetime-local input's value names, read in the browser's time zone,
 * as the API takes it (RFC 3339, in UTC); undefined for an empty or invalid value.
 */
export function instantOf(local: string): string | undefined {
  if (!local) return undefined
  const d = new Date(local)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
}

/** when shows an instant in the browser's time zone, to the minute. */
export function when(iso: string): string {
  return new Date(iso).toLocaleString([], { dateStyle: "medium", timeStyle: "short" })
}

/** onBehalf is how a review names the person on whose behalf it was made: "" for one's own. */
export function onBehalf(onBehalfOf?: string): string {
  return onBehalfOf ? ` on behalf of ${onBehalfOf}` : ""
}

/** approverLabel names an approver of a rule, with the person on whose behalf they approved. */
export function approverLabel(approver: string, onBehalfOf?: Record<string, string> | null): string {
  const o = onBehalfOf?.[approver]
  return o ? `${approver} (on behalf of ${o})` : approver
}

/** Capacity is how someone may review a request: in their own right (""), or for a delegator. */
export type Capacity = { value: string; label: string; delegation?: ReviewForView }

/**
 * capacitiesOf lists how the signed-in person may review a request now: in their own right when
 * the server says so (canReviewOwn; a server that says only canReview, with nobody to review for,
 * means that too), then on behalf of each delegator whose approvals they hold.
 */
export function capacitiesOf(r: RequestView): Capacity[] {
  const delegators = r.reviewFor ?? []
  const own = r.canReviewOwn || (r.canReview && delegators.length === 0)
  return [
    ...(own ? [{ value: "", label: "Yourself" }] : []),
    ...delegators.map((d) => ({ value: d.delegator, label: `On behalf of ${d.delegator}`, delegation: d })),
  ]
}
