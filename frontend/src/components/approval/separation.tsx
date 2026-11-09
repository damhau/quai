import { UserX } from "lucide-react"
import type { SeparationView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"
import { shortSha } from "@/lib/format"

/** How many identities that match no one are listed; the rest are counted. */
const shown = 10

/**
 * Separation names who cannot approve a request under the repository's separation of duties: the
 * people who changed its definitions in the period before it (definitionAuthors), with their
 * commits, and the commits' identities that match no one in Quai. On a request, it is the
 * separation it was planned with, which a later change of the repository's options does not
 * move. destroys: requireTwo shows as a rule of the approval chain.
 */
export function Separation({ separation, submitted = false }: { separation: SeparationView; submitted?: boolean }) {
  if (!separation.definitionAuthors) return null
  const authors = separation.authors ?? []
  const unmatched = separation.unmatched ?? []
  return (
    <section className="grid gap-1 text-sm" aria-label="Separation of duties">
      <span className="text-muted-foreground text-xs">
        Changed its definitions in the {separation.definitionAuthors} before it{submitted ? " was planned" : ""}, so cannot approve
      </span>
      <span className="flex flex-wrap gap-1">
        {authors.map((a) => (
          <Badge key={a.name} variant="outline">
            {a.name}
            <span className="text-muted-foreground font-mono">{(a.commits ?? []).map(shortSha).join(", ")}</span>
          </Badge>
        ))}
        {!authors.length && <span className="text-muted-foreground">No one Quai knows</span>}
      </span>
      {!separation.from && (
        <span className="text-muted-foreground text-xs">Quai had recorded no definitions when the period began: their whole history counts.</span>
      )}
      {unmatched.length > 0 && (
        <p className="flex items-start gap-1 text-xs text-amber-700 dark:text-amber-300">
          <UserX className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          <span>
            Match no one in Quai: {unmatched.slice(0, shown).join(", ")}
            {unmatched.length > shown && ` and ${unmatched.length - shown} more`}. They exclude no one now; whoever signs in with one of their
            emails, verified, cannot approve either.
          </span>
        </p>
      )}
    </section>
  )
}
