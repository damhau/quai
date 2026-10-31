import { History } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"

/**
 * PinsWarning says why a blueprint cannot be requested: some of its resources pin a range of
 * their type's versions the type has left, a major version
 * (docs/design/02-resource-types-and-generators.md#schema-evolution). Nothing when there is
 * nothing to say.
 */
export function PinsWarning({ pins }: { pins?: string[] | null }) {
  if (!pins?.length) return null
  return (
    <Alert variant="destructive" role="alert">
      <History />
      <AlertTitle>This blueprint must be updated for a new version of its types</AlertTitle>
      <AlertDescription>
        <ul className="list-disc pl-4">
          {pins.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
      </AlertDescription>
    </Alert>
  )
}
