import { BellOff, CircleCheck, CircleMinus, Clock, FileClock } from "lucide-react"
import type { RuleView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"
import { approverLabel } from "@/lib/delegations"
import { amongOthers } from "@/lib/requests"

/** ApprovalChain shows each approval rule of a request with its progress. */
export function ApprovalChain({ rules }: { rules: RuleView[] }) {
  if (!rules.length) return <p className="text-muted-foreground text-sm">No approval rule applies.</p>
  return (
    <ol className="grid gap-3">
      {rules.map((r) => {
        const approvals = r.approvals ?? []
        const others = r.overall && amongOthers(rules, r)
        return (
          <li key={(r.names ?? []).join(",")} className="flex items-start gap-2 text-sm">
            {r.notApplicable ? (
              <CircleMinus className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-label="Does not apply" />
            ) : r.waitingForPlan ? (
              <FileClock className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-label="Waiting for the plan" />
            ) : r.satisfied ? (
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-600" aria-label="Satisfied" />
            ) : (
              <Clock className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-label="Waiting" />
            )}
            <div className={`grid gap-1 ${r.notApplicable ? "text-muted-foreground" : ""}`}>
              <span>
                <span className={`font-medium ${r.notApplicable ? "line-through" : ""}`}>{(r.names ?? []).join(", ")}</span>: {approvals.length} of {r.min}
                {r.overall ? " different people" : ""} {others ? "among the approvers of the other rules" : "from"}
              </span>
              {r.when && (
                <span className="text-muted-foreground text-xs">
                  {r.waitingForPlan ? "Decided by the plan: " : r.notApplicable ? "Does not apply to the plan: " : "Applies to the plan: "}
                  <code className="font-mono">{r.when}</code>
                  {r.planError && <> ({r.planError})</>}
                </span>
              )}
              {!others && (
                <span className="flex flex-wrap gap-1">
                  {(r.approvers ?? []).map((a) => (
                    <Badge key={a} variant="outline">
                      {a}
                    </Badge>
                  ))}
                  {r.external && <Badge variant="outline">{r.external}</Badge>}
                </span>
              )}
              {approvals.length > 0 && (
                <span className="text-muted-foreground text-xs">Approved by {approvals.map((a) => approverLabel(a, r.onBehalfOf)).join(", ")}</span>
              )}
              {r.noOneToNotify && (
                <span className="text-muted-foreground flex items-center gap-1 text-xs">
                  <BellOff className="size-3.5 shrink-0" aria-hidden />
                  No one who may approve is notified: Quai knows no address of theirs on the channels it notifies on, or they turned
                  approvals off. Tell them yourself.
                </span>
              )}
            </div>
          </li>
        )
      })}
    </ol>
  )
}
