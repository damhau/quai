import { ShieldCheck, Zap } from "lucide-react"
import type { RuleView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"

/** RulesPreview lists who will have to approve a request. */
export function RulesPreview({ rules }: { rules: RuleView[] }) {
  if (!rules.length)
    return (
      <p className="text-muted-foreground flex items-center gap-2 text-sm">
        <Zap className="size-4" aria-hidden /> No approval needed: the change is committed as soon as you submit.
      </p>
    )
  return (
    <ul className="grid gap-2">
      {rules.map((r) => (
        <li key={(r.names ?? []).join(",")} className="flex items-start gap-2 text-sm">
          <ShieldCheck className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-hidden />
          <div className="grid gap-1">
            <span>
              <span className="font-medium">{(r.names ?? []).join(", ")}</span>: {r.min} approval{r.min === 1 ? "" : "s"} from
            </span>
            <span className="flex flex-wrap gap-1">
              {(r.approvers ?? []).map((a) => (
                <Badge key={a} variant="outline">
                  {a}
                </Badge>
              ))}
              {r.external && <Badge variant="outline">{r.external}</Badge>}
            </span>
            {r.waitingForPlan && r.when && (
              <span className="text-muted-foreground text-xs">
                Decided by the plan: <code className="font-mono">{r.when}</code>
              </span>
            )}
          </div>
        </li>
      ))}
    </ul>
  )
}
