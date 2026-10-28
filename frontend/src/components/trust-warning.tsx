import { ShieldAlert } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"

/**
 * TrustWarning says, in red, why an item's definitions can change without review: its repository's
 * branch is unprotected, or no required reviewers cover the definitions folder
 * (docs/design/01-repository-connections.md#trust). Nothing when there is nothing to say.
 */
export function TrustWarning({ problems }: { problems?: string[] | null }) {
  if (!problems?.length) return null
  return (
    <Alert variant="destructive" role="alert">
      <ShieldAlert />
      <AlertTitle>These definitions can change without review</AlertTitle>
      <AlertDescription>
        <ul className="list-disc pl-4">
          {problems.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
      </AlertDescription>
    </Alert>
  )
}
