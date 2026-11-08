import type { AuditActorView, AuditEventView, AuditObjectView, MeOutputBody } from "@/api/generated/types.gen"

// The audit log (docs/design/09-security-and-compliance.md#audit-log): what its page says about
// events. Personal data is in an event as a pseudonym (pd:…); the page carries the clear values
// that were not deleted.

export type Personal = Record<string, string>

/** canReadAudit says whether someone may read the audit log: auditors and platform admins. */
export function canReadAudit(me: MeOutputBody | undefined): boolean {
  return (me?.roles ?? []).some((r) => r.role === "auditor" || r.role === "platform-admin")
}

const pseudonym = /^pd:[0-9a-f]{64}$/

/** erased is what the page shows for personal data whose clear value was deleted. */
export const erased = "(erased)"

/** clear returns a value with its personal data in clear. */
export function clear(value: string, personal: Personal): string {
  if (!pseudonym.test(value)) return value
  return personal[value] ?? erased
}

const labels: Record<string, string> = {
  "auth.login": "Signed in",
  "auth.logout": "Signed out",
  "auth.grants_changed": "Roles changed",
  "token.created": "API token created",
  "token.revoked": "API token revoked",
  "run_token.created": "Run token generated",
  "request.submitted": "Request submitted",
  "request.reviewed": "Request reviewed",
  "request.approved": "Request approved",
  "request.rejected": "Request rejected",
  "request.cancelled": "Request cancelled",
  "request.retried": "Request retried",
  "request.stale": "Request stale",
  "request.merged": "Request merged",
  "request.merged_without_approval": "Merged without approval",
  "request.applied": "Request applied",
  "request.failed": "Request failed",
  "repository.created": "Repository connected",
  "repository.updated": "Repository changed",
  "repository.deleted": "Repository disconnected",
  "repository.enforcement_lost": "Approvals no longer enforced",
  "repository.enforcement_restored": "Approvals enforced again",
  "definitions.commit": "Definitions changed",
  "audit.verified": "Audit log verified",
  "audit.broken": "Audit log does not verify",
  "audit.pruned": "Audit log pruned",
}

/** actionLabel names an action for people; an action this version does not know keeps its code. */
export const actionLabel = (action: string) => labels[action] ?? action

/** alarming says whether an action is one an auditor looks at first. */
export const alarming = (action: string) =>
  action === "request.merged_without_approval" || action === "repository.enforcement_lost" || action === "audit.broken"

/** The filter's groups of actions, by prefix. */
export const actionGroups = [
  { value: "auth", label: "Sign-ins and roles" },
  { value: "token", label: "API tokens" },
  { value: "run_token", label: "Run tokens" },
  { value: "request", label: "Requests" },
  { value: "repository", label: "Repositories" },
  { value: "definitions", label: "Definitions" },
  { value: "audit", label: "The audit log itself" },
]

/** actorLabel says who acted. */
export function actorLabel(a: AuditActorView, personal: Personal): string {
  switch (a.type) {
    case "system":
      return "Quai"
    case "runner":
      return `Runner of ${a.id}`
    case "token":
      return `${clear(a.id, personal)} (API token)`
    default:
      return clear(a.id, personal)
  }
}

/** objectLabel says what an event is about. */
export function objectLabel(o: AuditObjectView, personal: Personal): string {
  if (o.type === "user") return clear(o.id, personal)
  if (o.type === "audit") return "Audit log"
  if (o.type === "request") return o.resource ? o.resource : o.id
  return o.id
}

/** detailText renders a value of an event's details, its personal data in clear. */
export function detailText(v: unknown, personal: Personal): string {
  if (v === null || v === undefined) return ""
  if (typeof v === "string") return clear(v, personal)
  if (Array.isArray(v) && v.every((e) => typeof e === "string")) return v.map((e) => clear(e, personal)).join(", ")
  if (typeof v === "object") return JSON.stringify(v, (_, e: unknown) => (typeof e === "string" ? clear(e, personal) : e))
  return String(v)
}

/** details are an event's details to show, in a stable order, without the empty ones. */
export function details(e: AuditEventView, personal: Personal): [string, string][] {
  return Object.keys(e.details ?? {})
    .sort()
    .map((k): [string, string] => [k, detailText(e.details[k], personal)])
    .filter(([, v]) => v !== "" && v !== "[]")
}

/** exportURL downloads the audit log, to verify it offline with quai audit verify. */
export const exportURL = "/api/v1/audit/export"

/** dayStart is the start of a local day (yyyy-mm-dd) as an RFC 3339 time; dayEnd the start of the next. */
export function dayStart(day: string): string | undefined {
  const d = new Date(`${day}T00:00`)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
}

export function dayEnd(day: string): string | undefined {
  const d = new Date(`${day}T00:00`)
  if (Number.isNaN(d.getTime())) return undefined
  d.setDate(d.getDate() + 1)
  return d.toISOString()
}
