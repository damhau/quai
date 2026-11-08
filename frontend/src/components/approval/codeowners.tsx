import { UserX } from "lucide-react"
import type { CodeownersView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"

/**
 * Codeowners lists the code owners of a change's files as Quai matches them, and those the
 * platform names that match no one in Quai: they cannot approve here until the repository maps them.
 */
export function Codeowners({ codeowners }: { codeowners: CodeownersView }) {
  const owners = codeowners.owners ?? []
  const unmatched = codeowners.unmatched ?? []
  return (
    <section className="grid gap-1 text-sm" aria-label="Code owners">
      <span className="text-muted-foreground text-xs">Code owners of the changed files</span>
      <span className="flex flex-wrap gap-1">
        {owners.map((o) => (
          <Badge key={o} variant="outline">
            {o}
          </Badge>
        ))}
        {!owners.length && <span className="text-muted-foreground">None that Quai can match</span>}
      </span>
      {unmatched.length > 0 && <UnmatchedOwners owners={unmatched} />}
    </section>
  )
}

/** UnmatchedOwners lists owners the platform names that match no one in Quai. */
export function UnmatchedOwners({ owners }: { owners: string[] }) {
  return (
    <p className="flex items-start gap-1 text-xs text-amber-700 dark:text-amber-300">
      <UserX className="mt-0.5 size-3.5 shrink-0" aria-hidden />
      <span>
        Match no one in Quai: {owners.join(", ")}. An administrator maps them to Quai users or groups in the repository&apos;s{" "}
        <code className="font-mono">spec.codeowners.mapping</code>.
      </span>
    </p>
  )
}
