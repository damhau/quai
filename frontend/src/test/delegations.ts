import { http, HttpResponse } from "msw"
import type { DelegateBody, DelegationView } from "@/api/generated/types.gen"

// A fake of delegations of approvals, as the server checks them: the delegator from the session
// (never from the body), a colleague who signed in, not oneself, one delegate at a time; only the
// delegator revokes. stepUp says that making one needs a recent sign-in.

/** signedIn are the people who signed in to the fake Quai. */
const signedIn = ["alice", "bob", "fiona", "dana"]

export const delegations: {
  list: DelegationView[]
  posts: { user: string; csrf: string | null; body: DelegateBody }[]
  stepUp: boolean
} = { list: [], posts: [], stepUp: false }

export function resetDelegations() {
  Object.assign(delegations, { list: [], posts: [], stepUp: false })
}

const stepUpRefusal = {
  type: "https://quai.dev/problems/step-up-required",
  title: "Forbidden",
  status: 403,
  detail: "this action needs a recent sign-in: you authenticated more than 5m0s ago; sign in again",
}

const invalid = (location: string, message: string) =>
  HttpResponse.json({ status: 422, detail: "some fields need attention", errors: [{ location, message }] }, { status: 422, headers: { "Content-Type": "application/problem+json" } })

/** delegationHandlers serves delegations to user(); steppedUp says whether their sign-in is recent. */
export const delegationHandlers = (user: () => string | null, steppedUp: () => boolean) => [
  http.get("*/api/v1/me/delegations", () => {
    const u = user()
    if (!u) return HttpResponse.json({ status: 401, detail: "not signed in" }, { status: 401 })
    return HttpResponse.json({ delegations: delegations.list.filter((d) => d.delegator === u || d.delegate === u), maxDays: 90, stepUp: delegations.stepUp })
  }),
  http.post("*/api/v1/me/delegations", async ({ request }) => {
    const u = user()
    if (!u) return HttpResponse.json({ status: 401, detail: "not signed in" }, { status: 401 })
    const body = (await request.json()) as DelegateBody
    const csrf = request.headers.get("X-CSRF-Token")
    delegations.posts.push({ user: u, csrf, body })
    if (csrf !== `csrf-${u}`) return HttpResponse.json({ status: 403, detail: "missing or wrong X-CSRF-Token header" }, { status: 403 })
    if (delegations.stepUp && !steppedUp()) return HttpResponse.json(stepUpRefusal, { status: 403, headers: { "Content-Type": "application/problem+json" } })
    if (body.delegate === u) return invalid("body.delegate", "is you: choose a colleague")
    if (!signedIn.includes(body.delegate)) return invalid("body.delegate", "is no one who has signed in to Quai: they must sign in once first")
    const starts = Date.parse(body.startsAt)
    const ends = Date.parse(body.endsAt)
    if (Number.isNaN(ends) || ends <= starts) return invalid("body.endsAt", "must be after the start")
    if (ends - starts > 90 * 24 * 3600 * 1000) return invalid("body.endsAt", "is more than 90 days after the start: a delegation lasts 90 days at most")
    const mine = delegations.list.find((d) => d.delegator === u && Date.parse(d.startsAt) < ends && starts < Date.parse(d.endsAt))
    if (mine)
      return HttpResponse.json(
        { status: 409, detail: `conflict: you have delegated your approvals to ${mine.delegate} for part of that period: revoke that delegation first` },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      )
    const d: DelegationView = {
      id: `00000000-0000-4000-8000-${String(delegations.list.length + 1).padStart(12, "0")}`,
      delegator: u,
      delegate: body.delegate,
      startsAt: body.startsAt,
      endsAt: body.endsAt,
      createdAt: new Date().toISOString(),
      active: starts <= Date.now(),
    }
    delegations.list.push(d)
    return HttpResponse.json(d, { status: 201 })
  }),
  http.delete("*/api/v1/delegations/:id", ({ params }) => {
    const u = user()
    const i = delegations.list.findIndex((d) => d.id === params.id && d.delegator === u)
    if (i < 0) return HttpResponse.json({ status: 404, detail: "not found: no such delegation of yours" }, { status: 404 })
    delegations.list.splice(i, 1)
    return new HttpResponse(null, { status: 204 })
  }),
]
