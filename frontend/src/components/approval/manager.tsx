import { UserX } from "lucide-react"
import type { ManagerView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"

/**
 * Manager names the requester's manager, who approves for a rule whose approvers include manager:
 * on a request, the manager when it was submitted, which a later change does not move. Without one,
 * it says why: nobody can satisfy the rule.
 */
export function Manager({ manager, submitted = false }: { manager: ManagerView; submitted?: boolean }) {
  return (
    <section className="grid gap-1 text-sm" aria-label="Manager">
      <span className="text-muted-foreground text-xs">{submitted ? "The requester's manager when it was submitted" : "Your manager, who approves for a manager rule"}</span>
      {manager.name ? (
        <span className="flex flex-wrap gap-1">
          <Badge variant="outline">{manager.name}</Badge>
        </span>
      ) : (
        <p className="flex items-start gap-1 text-xs text-amber-700 dark:text-amber-300">
          <UserX className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          <span>No manager, so nobody can satisfy a manager rule: {manager.reason || "Quai recorded none"}.</span>
        </p>
      )}
    </section>
  )
}
