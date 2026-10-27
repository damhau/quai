import type { EventView, RequestView } from "@/api/generated/types.gen"
import { shortSha } from "@/lib/format"

// How requests read in the UI. States and event kinds are those of backend/internal/requests.

type Tone = "neutral" | "waiting" | "good" | "bad"

export const states: Record<string, { label: string; tone: Tone }> = {
  pending_approval: { label: "Pending approval", tone: "waiting" },
  approved: { label: "Approved", tone: "neutral" },
  merging: { label: "Delivering", tone: "neutral" },
  applied: { label: "Applied", tone: "good" },
  rejected: { label: "Denied", tone: "bad" },
  failed: { label: "Failed", tone: "bad" },
  stale: { label: "Stale", tone: "waiting" },
  cancelled: { label: "Cancelled", tone: "neutral" },
}

/** inProgress reports whether a request may still change by itself, so its page refreshes. */
export const inProgress = (state: string) => ["pending_approval", "approved", "merging"].includes(state)

export function requestTitle(r: Pick<RequestView, "kind" | "action" | "target">): string {
  switch (r.kind) {
    case "create":
      return `Create ${r.target}`
    case "update":
      return `Update ${r.target}`
    case "delete":
      return `Delete ${r.target}`
    case "action":
      return `${r.action ?? "Action"} on ${r.target}`
  }
  return r.target
}

function str(data: unknown, key: string): string | undefined {
  const v = data !== null && typeof data === "object" ? (data as Record<string, unknown>)[key] : undefined
  return typeof v === "string" ? v : undefined
}

/** eventText says what an event of a request's timeline means. */
export function eventText(e: EventView): string {
  const d = e.data
  switch (e.kind) {
    case "submitted":
      return "submitted the request"
    case "approved":
      return str(d, "reason") ? `approved: ${str(d, "reason")}` : "all approvals are in"
    case "rejected":
      return "denied the request"
    case "stale":
      return `the files changed since the plan${str(d, "error") ? `: ${str(d, "error")}` : ""}; plan the request again`
    case "merging":
      return "delivering the change to Git"
    case "committed":
      return `committed ${shortSha(str(d, "commit") ?? "")}`
    case "applied":
      return `applied ${shortSha(str(d, "commit") ?? "")}${str(d, "note") ? ` (${str(d, "note")})` : ""}`
    case "failed":
      return `delivery failed: ${str(d, "error") ?? "unknown error"}`
    case "cancelled":
      return "cancelled the request"
    case "retried":
      return "retried the delivery"
  }
  return e.kind
}
