import { Lock } from "lucide-react"
import type { EntitlementsView } from "@/api/generated/types.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { entitlementLines } from "@/lib/entitlements"

/** EntitlementsBadge marks a restricted catalog item; its title lists who sees and requests it. */
export function EntitlementsBadge({ entitlements }: { entitlements?: EntitlementsView | null }) {
  if (!entitlements) return null
  return (
    <Badge variant="outline" title={entitlementLines(entitlements).join("\n")}>
      <Lock aria-hidden /> Restricted
    </Badge>
  )
}

/**
 * EntitlementsNote, above a request form, says who may request the item and, when the caller may
 * not, why: everywhere, or in the environment the form is for. Nothing when the item restricts no
 * one.
 */
export function EntitlementsNote({ entitlements, env }: { entitlements?: EntitlementsView | null; env?: string }) {
  if (!entitlements) return null
  const chosen = (entitlements.environments ?? []).find((e) => env && e.environment.toLowerCase() === env.trim().toLowerCase())
  const closed = entitlements.canRequest === false ? entitlements.reason || "this catalog item is not open to your groups" : chosen?.canRequest === false ? `this catalog item is not open to your groups in ${chosen.environment}` : undefined
  return (
    <Alert variant={closed ? "destructive" : "default"} role={closed ? "alert" : "note"} aria-label="Who may request this item">
      <Lock />
      <AlertTitle>{closed ? `You cannot request this: ${closed}` : "Restricted item"}</AlertTitle>
      <AlertDescription>
        <ul className="list-disc pl-4">
          {entitlementLines(entitlements).map((l) => (
            <li key={l}>{l}</li>
          ))}
        </ul>
      </AlertDescription>
    </Alert>
  )
}
