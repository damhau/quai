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
/** planDecisions says which rules a plan made apply, from a plan_decided event. */
function planDecisions(d: unknown): string {
  const decisions = (d as { decisions?: { rules?: string[]; applies?: boolean }[] } | null)?.decisions ?? []
  return decisions.map((x) => `${(x.rules ?? []).join(", ")} ${x.applies ? "applies" : "does not apply"}`).join("; ") || "none"
}

export function eventText(e: EventView): string {
  const d = e.data
  switch (e.kind) {
    case "submitted":
      return "submitted the request"
    case "approved":
      return str(d, "reason") ? `approved: ${str(d, "reason")}` : "all approvals are in"
    case "approve":
      return "approved"
    case "deny":
      return `denied${str(d, "comment") ? `: ${str(d, "comment")}` : ""}`
    case "rejected":
      return "denied the request"
    case "stale":
      if (str(d, "definitions")) return `${str(d, "definitions")} since the plan; plan the request again`
      if (str(d, "region")) return `${str(d, "region")} changed on the base branch; plan the request again`
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
      return str(d, "reason") ? `cancelled: ${str(d, "reason")}` : "cancelled the request"
    case "pull_request_opened":
      return `opened pull request #${str(d, "pullRequest") ?? ""}`
    case "rebuilt":
      return `rebuilt the pull request on the new base ${shortSha(str(d, "base") ?? "")}; the approvals stand`
    case "modified_outside_quai":
      return str(d, "reason") ?? "the pull request was changed outside Quai"
    case "merged":
      return `the pull request was merged as ${shortSha(str(d, "commit") ?? "")}`
    case "merged_without_approval":
      return `the pull request was merged without Quai's approval, as ${shortSha(str(d, "commit") ?? "")}: the enforcement was bypassed`
    case "approvals_commented":
      return "recorded the approvals on the pull request"
    case "retried":
      return "retried the delivery"
    case "plan_decided":
      return `the plan decided the rules that read it: ${planDecisions(d)}`
    case "approval_needed":
      return `needs approval again: ${str(d, "reason") ?? "the rules changed"}`
  }
  return e.kind
}
