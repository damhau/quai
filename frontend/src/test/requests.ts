import { http, HttpResponse } from "msw"
import type { RequestView } from "@/api/generated/types.gen"

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

/** as returns the request as the signed-in user sees it. */
function as(user: string | null, r: RequestView): RequestView {
  const open = ["pending_approval", "approved", "stale", "failed"].includes(r.state)
  const approver = user !== null && user in approverOf
  return { ...r, canReview: r.state === "pending_approval" && approver, canCancel: open && (user === "bob" || user === "alice"), canRetry: r.state === "failed" && user === "bob" }
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
 * decision ("" when only opening it): a fake link reads "fake.<approver>.<decision>", or
 * "fake.expired". It returns the server's refusal, or the decision the link opens.
 */
function chatLinkRefusal(user: string | null, link: string, decision: string) {
  const refuse = (reason: string) =>
    HttpResponse.json({ type: "https://quai.dev/problems/chat-link-refused", title: "Forbidden", status: 403, detail: `the link is refused: ${reason}` }, { status: 403 })
  const [, approver, linked] = link.split(".")
  if (link === "fake.expired") return refuse("this link expired at 2026-10-28T10:00:00Z: review the request here")
  if (!link.startsWith("fake.") || !approver || !linked) return refuse("this link is not one Quai made")
  if (approver !== user) return refuse("this link was sent to someone else: review the request here as yourself")
  if (decision !== "" && decision !== linked) return refuse("the decision is not the link's: review the request here")
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
      return HttpResponse.json({ decision, channel: "teams", expiresAt: "2026-10-28T10:00:00Z" })
    }),
    http.post("*/api/v1/requests/:id/review", async ({ request }) => {
      const { decision, comment, link } = (await request.json()) as { decision: string; comment?: string; link?: string }
      // As the server: the link is checked first, then everything a review checks.
      const refusal = link === undefined ? undefined : chatLinkRefusal(user(), link, decision)
      if (refusal !== undefined && typeof refusal !== "string") return refusal
      if (requests.current.stepUp && !steppedUp()) return HttpResponse.json(stepUpRefusal, { status: 403 })
      if (decision === "deny" && !comment)
        return HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location: "body.comment", message: "is required to deny: say what to change" }] }, { status: 422 })
      requests.links.push(link)
      const r = requests.current
      const approve = decision === "approve"
      const rules = (r.rules ?? []).map((x, i) => (approve && i === approverOf[user() ?? ""] ? { ...x, approvals: [user() ?? ""], satisfied: true } : x))
      requests.current = {
        ...r,
        state: !approve ? "rejected" : rules.every((x) => x.satisfied) ? "approved" : "pending_approval",
        rules,
        reviews: [...(r.reviews ?? []), { reviewer: user() ?? "", decision, comment, at: now, stepUp: r.stepUp || undefined }],
        events: [
          ...(r.events ?? []),
          approve ? { kind: "approve", actor: user() ?? "", data: null, at: now } : { kind: "rejected", actor: user() ?? "", data: null, at: now },
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
