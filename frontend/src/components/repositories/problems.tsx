import { StepUpRefusal } from "@/components/step-up-prompt"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { isProblem, isStepUpRequired, problemMessage } from "@/lib/problems"

/**
 * Problems shows an API error, with each problem of the document it lists; a change refused for
 * want of a recent sign-in offers to sign in again, which clears the error (reset).
 */
export function Problems({ error, reset }: { error: unknown; reset?: () => void }) {
  if (isStepUpRequired(error)) return <StepUpRefusal error={error} reset={reset} />
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
