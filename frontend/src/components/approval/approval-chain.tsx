import { CircleCheck, Clock } from "lucide-react"
import type { RuleView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"

/** ApprovalChain shows each approval rule of a request with its progress. */
export function ApprovalChain({ rules }: { rules: RuleView[] }) {
  if (!rules.length) return <p className="text-muted-foreground text-sm">No approval rule applies.</p>
  return (
    <ol className="grid gap-3">
      {rules.map((r) => {
        const approvals = r.approvals ?? []
        return (
          <li key={(r.names ?? []).join(",")} className="flex items-start gap-2 text-sm">
            {r.satisfied ? (
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-600" aria-label="Satisfied" />
            ) : (
              <Clock className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-label="Waiting" />
            )}
            <div className="grid gap-1">
              <span>
                <span className="font-medium">{(r.names ?? []).join(", ")}</span>: {approvals.length} of {r.min} from
              </span>
              <span className="flex flex-wrap gap-1">
                {(r.approvers ?? []).map((a) => (
                  <Badge key={a} variant="outline">
                    {a}
                  </Badge>
                ))}
                {r.external && <Badge variant="outline">{r.external}</Badge>}
              </span>
              {approvals.length > 0 && <span className="text-muted-foreground text-xs">Approved by {approvals.join(", ")}</span>}
            </div>
          </li>
        )
      })}
    </ol>
  )
}
