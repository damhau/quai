import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { isProblem, problemMessage } from "@/lib/problems"

/** Problems shows an API error, with each problem of the document it lists. */
export function Problems({ error }: { error: unknown }) {
  const details = isProblem(error) ? (error.errors ?? []).map((d) => d.message ?? "") : []
  return (
    <Alert variant="destructive" role="alert">
      <AlertTitle>{problemMessage(error)}</AlertTitle>
      {details.length > 0 && (
        <AlertDescription>
          <ul className="list-disc pl-4">
            {details.map((d) => (
              <li key={d}>{d}</li>
            ))}
          </ul>
        </AlertDescription>
      )}
    </Alert>
  )
}
