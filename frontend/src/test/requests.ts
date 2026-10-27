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

export const requests: { current: RequestView } = { current: pendingVM() }

// alice approves for the platform admins, fiona for FinOps.
const approverOf: Record<string, number> = { alice: 0, fiona: 1 }

/** as returns the request as the signed-in user sees it. */
function as(user: string | null, r: RequestView): RequestView {
  const open = ["pending_approval", "approved", "stale", "failed"].includes(r.state)
  const approver = user !== null && user in approverOf
  return { ...r, canReview: r.state === "pending_approval" && approver, canCancel: open && (user === "bob" || user === "alice"), canRetry: r.state === "failed" && user === "bob" }
}

export function requestHandlers(user: () => string | null) {
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
    http.post("*/api/v1/requests/:id/review", async ({ request }) => {
      const { decision, comment } = (await request.json()) as { decision: string; comment?: string }
      if (decision === "deny" && !comment)
        return HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location: "body.comment", message: "is required to deny: say what to change" }] }, { status: 422 })
      const r = requests.current
      const approve = decision === "approve"
      const rules = (r.rules ?? []).map((x, i) => (approve && i === approverOf[user() ?? ""] ? { ...x, approvals: [user() ?? ""], satisfied: true } : x))
      requests.current = {
        ...r,
        state: !approve ? "rejected" : rules.every((x) => x.satisfied) ? "approved" : "pending_approval",
        rules,
        reviews: [...(r.reviews ?? []), { reviewer: user() ?? "", decision, comment, at: now }],
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
