import { http, HttpResponse } from "msw"
import type { RequestView, ReviewForView } from "@/api/generated/types.gen"
import { delegations } from "./delegations"

// A fake backend for requests: bob's VM in prod waits for alice, a platform admin; reviews and
// cancellations change it as the server would.

const now = "2026-10-27T10:00:00Z"

export function pendingVM(): RequestView {
  return {
    id: "11111111-1111-4111-8111-111111111111",
    number: 3,
    kind: "create",
    target: "infra-live/vm/prod/web09",
    repository: "infra-live",
    type: "vm",
    requester: "bob",
    reason: "Capacity for the launch",
    state: "pending_approval",
    stepUp: false,
    stack: "envs/prod",
    inputs: { cpu: 2 },
    createdAt: now,
    updatedAt: now,
    canReview: false,
    canCancel: false,
    canRetry: false,
    rules: [
      { names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: [], satisfied: false },
      { names: ["type/large-vm"], approvers: ["group:finops"], min: 1, approvals: [], satisfied: false },
    ],
    reviews: [],
    changes: [{ path: "envs/prod/vms/web09.yaml", op: "create", diff: [{ op: "+", text: "cpu: 2" }] }],
    events: [{ kind: "submitted", actor: "bob", data: null, at: now }],
  }
}

// links are the links of chat cards the reviews carried, in order.
export const requests: { current: RequestView; links: (string | undefined)[] } = { current: pendingVM(), links: [] }

// alice approves for the platform admins, fiona for FinOps.
const approverOf: Record<string, number> = { alice: 0, fiona: 1 }

/**
 * reviewFor lists the approvers whose approvals someone holds now through a delegation, and may
 * review a request for: neither of them its requester, the delegator not having reviewed it.
 */
function reviewFor(user: string | null, r: RequestView): ReviewForView[] {
  if (user === null || user === r.requester || r.state !== "pending_approval") return []
  return delegations.list
    .filter((d) => d.delegate === user && d.active && d.delegator in approverOf && d.delegator !== r.requester)
    .filter((d) => !(r.reviews ?? []).some((v) => v.reviewer === d.delegator))
    .map((d) => ({ delegator: d.delegator, delegation: d.id, until: d.endsAt }))
}

/** as returns the request as the signed-in user sees it. */
function as(user: string | null, r: RequestView): RequestView {
  const open = ["pending_approval", "approved", "stale", "failed"].includes(r.state)
  const own = r.state === "pending_approval" && user !== null && user in approverOf
  const delegated = reviewFor(user, r)
  return {
    ...r,
    canReview: own || delegated.length > 0,
    canReviewOwn: own || undefined,
    reviewFor: delegated.length > 0 ? delegated : undefined,
    canCancel: open && (user === "bob" || user === "alice"),
    canRetry: r.state === "failed" && user === "bob",
  }
}

/** stepUpRefusal is the server's answer to a review that needs a recent sign-in. */
const stepUpRefusal = {
  type: "https://quai.dev/problems/step-up-required",
  title: "Forbidden",
  status: 403,
  detail: "this action needs a recent sign-in: you authenticated more than 5m0s ago; sign in again",
}

/**
 * chatLinkRefusal checks a link of a chat card as the server does, for the signed-in user and a
 * decision ("" when only opening it), on someone's behalf: a fake link reads
 * "fake.<approver>.<decision>", "fake.<approver>.<decision>.<delegator>" for a delegate, or
 * "fake.expired". It returns the server's refusal, or the decision the link opens.
 */
function chatLinkRefusal(user: string | null, link: string, decision: string, onBehalfOf = "") {
  const refuse = (reason: string) =>
    HttpResponse.json({ type: "https://quai.dev/problems/chat-link-refused", title: "Forbidden", status: 403, detail: `the link is refused: ${reason}` }, { status: 403 })
  const [, approver, linked, behalf = ""] = link.split(".")
  if (link === "fake.expired") return refuse("this link expired at 2026-10-28T10:00:00Z: review the request here")
  if (!link.startsWith("fake.") || !approver || !linked) return refuse("this link is not one Quai made")
  if (approver !== user) return refuse("this link was sent to someone else: review the request here as yourself")
  if (decision !== "" && decision !== linked) return refuse("the decision is not the link's: review the request here")
  if (decision !== "" && onBehalfOf !== behalf) return refuse("the link is for a decision on someone else's behalf: review the request here")
  if (requests.current.state !== "pending_approval") return refuse("the request no longer waits for approval")
  return linked
}

/** requestHandlers serves requests to user(); steppedUp says whether their sign-in is recent. */
export function requestHandlers(user: () => string | null, steppedUp: () => boolean) {
  return [
    http.get("*/api/v1/requests", ({ request }) => {
      const view = new URL(request.url).searchParams.get("view") ?? "mine"
      const r = as(user(), requests.current)
      const reviewed = (r.reviews ?? []).some((v) => v.reviewer === user())
      const show = view === "all" || (view === "mine" && r.requester === user()) || (view === "inbox" && r.canReview && !reviewed)
      return HttpResponse.json(show ? [{ ...r, reviews: undefined, changes: undefined, events: undefined }] : [])
    }),
    http.get("*/api/v1/requests/:id", ({ params }) =>
      params.id === requests.current.id ? HttpResponse.json(as(user(), requests.current)) : HttpResponse.json({ status: 404, detail: "no such request" }, { status: 404 }),
    ),
    http.post("*/api/v1/requests/:id/chat-link", async ({ request }) => {
      const { link } = (await request.json()) as { link: string }
      const decision = chatLinkRefusal(user(), link, "")
      if (typeof decision !== "string") return decision
      const onBehalfOf = link.split(".")[3]
      return HttpResponse.json({ decision, channel: "teams", expiresAt: "2026-10-28T10:00:00Z", onBehalfOf })
    }),
    http.post("*/api/v1/requests/:id/review", async ({ request }) => {
      const { decision, comment, link, onBehalfOf } = (await request.json()) as { decision: string; comment?: string; link?: string; onBehalfOf?: string }
      // As the server: the link is checked first, then everything a review checks.
      const refusal = link === undefined ? undefined : chatLinkRefusal(user(), link, decision, onBehalfOf)
      if (refusal !== undefined && typeof refusal !== "string") return refusal
      const viewed = as(user(), requests.current)
      const delegation = onBehalfOf ? viewed.reviewFor?.find((f) => f.delegator === onBehalfOf) : undefined
      if (onBehalfOf ? !delegation : !viewed.canReviewOwn)
        return HttpResponse.json({ status: 403, detail: "you are not an approver of this request" }, { status: 403, headers: { "Content-Type": "application/problem+json" } })
      if (requests.current.stepUp && !steppedUp()) return HttpResponse.json(stepUpRefusal, { status: 403 })
      if (decision === "deny" && !comment)
        return HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location: "body.comment", message: "is required to deny: say what to change" }] }, { status: 422 })
      requests.links.push(link)
      const r = requests.current
      const approve = decision === "approve"
      // One person, one approval: the reviewer's earlier approval, in either capacity, is replaced.
      const counted = onBehalfOf ?? user() ?? ""
      const rules = (r.rules ?? []).map((x, i) => {
        const kept = (x.approvals ?? []).filter((a) => a !== user())
        const rule = { ...x, approvals: kept, satisfied: kept.length > 0, onBehalfOf: kept.length > 0 ? x.onBehalfOf : undefined }
        if (!approve || i !== approverOf[counted]) return rule
        return { ...rule, approvals: [user() ?? ""], satisfied: true, onBehalfOf: onBehalfOf ? { [user() ?? ""]: onBehalfOf } : undefined }
      })
      requests.current = {
        ...r,
        state: !approve ? "rejected" : rules.every((x) => x.satisfied) ? "approved" : "pending_approval",
        rules,
        reviews: [
          ...(r.reviews ?? []),
          { reviewer: user() ?? "", decision, comment, at: now, stepUp: r.stepUp || undefined, onBehalfOf, delegation: delegation?.delegation },
        ],
        events: [
          ...(r.events ?? []),
          approve
            ? { kind: "approve", actor: user() ?? "", data: onBehalfOf ? { onBehalfOf, delegation: delegation?.delegation } : null, at: now }
            : { kind: "rejected", actor: user() ?? "", data: onBehalfOf ? { onBehalfOf } : null, at: now },
          ...(approve && rules.every((x) => x.satisfied) ? [{ kind: "approved", data: null, at: now }] : []),
        ],
      }
      return HttpResponse.json(as(user(), requests.current))
    }),
    http.post("*/api/v1/requests/:id/cancel", () => {
      requests.current = { ...requests.current, state: "cancelled", events: [...(requests.current.events ?? []), { kind: "cancelled", actor: user() ?? "", data: { by: user() }, at: now }] }
      return HttpResponse.json(as(user(), requests.current))
    }),
  ]
}
