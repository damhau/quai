import { http, HttpResponse } from "msw"
import type { AuditCheckpointView, AuditEventView, AuditPageView, AuditVerificationView } from "@/api/generated/types.gen"

// A fake of the audit log: two pages of events, newest first, the queries it was asked, whether
// the database protects it, its latest checkpoint and the last verification.

const pd = (n: number) => `pd:${n.toString(16).padStart(64, "0")}`
const hash = (seq: number) => seq.toString(16).padStart(64, "a")

const checkpoint: AuditCheckpointView = { seq: 5, at: "2026-11-05T11:00:00.000Z", key: "c0ffee".padEnd(64, "0") }
const verified: AuditVerificationView = { seq: 6, at: "2026-11-05T12:00:00.000Z", verified: true, events: 5 }

export const audit = {
  queries: [] as URLSearchParams[],
  protected: true,
  checkpoint: checkpoint as AuditCheckpointView | undefined,
  verification: verified as AuditVerificationView | undefined,
}

const event = (seq: number, e: Partial<AuditEventView>): AuditEventView => ({
  seq,
  at: `2026-11-0${Math.min(seq, 9)}T10:00:00.000Z`,
  actor: { type: "system", id: "quai" },
  action: "request.applied",
  object: { type: "request", id: "11111111-1111-1111-1111-111111111111", resource: "infra-live/vm/prod/web01" },
  details: {},
  prev: hash(seq - 1),
  hash: hash(seq),
  ...e,
})

const newest: AuditEventView[] = [
  event(5, {
    action: "request.merged_without_approval",
    details: { from: "pending_approval", pullRequest: "7", commit: "abc123" },
  }),
  event(4, {
    actor: { type: "user", id: pd(1), ip: pd(2), authTime: "2026-11-04T09:58:00Z" },
    action: "request.reviewed",
    details: { decision: "approve", comment: pd(3), changeSet: "cs1" },
  }),
  event(3, {
    actor: { type: "token", id: pd(4), token: "tok-1" },
    action: "request.submitted",
    details: { kind: "action", reason: pd(5) },
  }),
]

const oldest: AuditEventView[] = [
  event(2, {
    actor: { type: "user", id: pd(6), authTime: "2026-11-02T09:00:00Z" },
    action: "auth.login",
    object: { type: "user", id: pd(7) },
    details: { method: "oidc", grants: ["requester@global"] },
  }),
]

export const auditHandlers = [
  http.get("*/api/v1/audit", ({ request }) => {
    const q = new URL(request.url).searchParams
    audit.queries.push(q)
    // alice's comment was deleted (pseudonymized): its pseudonym has no clear value any more.
    const personal = { [pd(1)]: "alice", [pd(2)]: "10.0.0.7", [pd(4)]: "bob", [pd(5)]: "Black Friday traffic", [pd(6)]: "carol", [pd(7)]: "carol" }
    const before = Number(q.get("before") ?? 0)
    const state = { protected: audit.protected, checkpoint: audit.checkpoint, verification: audit.verification }
    const page: AuditPageView =
      before === 3
        ? { events: oldest, personal, ...state }
        : { events: q.get("action") === "auth" ? oldest : newest, next: q.get("action") ? undefined : 3, personal, ...state }
    return HttpResponse.json(page)
  }),
]

export function resetAudit() {
  audit.queries = []
  audit.protected = true
  audit.checkpoint = checkpoint
  audit.verification = verified
}
