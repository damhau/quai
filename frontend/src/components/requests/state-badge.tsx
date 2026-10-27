import { Badge } from "@/components/ui/badge"
import { states } from "@/lib/requests"
import { resourceStatus } from "@/lib/resources"
import { cn } from "@/lib/utils"

const tones = {
  neutral: "",
  waiting: "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300",
  good: "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
  bad: "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-300",
}

export function StateBadge({ state }: { state: string }) {
  const s = states[state] ?? { label: state, tone: "neutral" as const }
  return (
    <Badge variant="outline" className={cn(tones[s.tone])}>
      {s.label}
    </Badge>
  )
}

export function ResourceStatusBadge({ status }: { status: string }) {
  const s = resourceStatus[status] ?? { label: status, tone: "neutral" as const }
  return (
    <Badge variant="outline" className={cn(tones[s.tone])}>
      {s.label}
    </Badge>
  )
}
