import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useLocation } from "@tanstack/react-router"
import { KeyRound } from "lucide-react"
import { getMeOptions, getMetaOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { stepUp } from "@/lib/auth"
import { problemMessage } from "@/lib/problems"

/**
 * StepUpPrompt asks the person to sign in again before an action that needs a recent sign-in
 * (step-up, docs/design/08-identity-access-tenancy.md#step-up). The server checks it when the
 * action is made; this only saves a refusal. With the identity provider the browser comes back to
 * this page; with the development switcher the same person signs in again here.
 */
export function StepUpPrompt({ title, reason, onSignedIn }: { title: string; reason?: string; onSignedIn?: () => void }) {
  const queryClient = useQueryClient()
  const { href } = useLocation()
  const { data: meta } = useQuery(getMetaOptions())
  const { data: me } = useQuery(getMeOptions())
  const signIn = useMutation({
    mutationFn: () => stepUp(queryClient, meta?.auth, me?.name ?? "", href),
    onSuccess: onSignedIn,
  })
  return (
    <Alert role="status">
      <KeyRound />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription className="grid gap-3">
        {reason && <p>{reason}</p>}
        {signIn.isError && <p className="text-destructive">{problemMessage(signIn.error)}</p>}
        <div>
          <Button type="button" size="sm" disabled={signIn.isPending || !me || !meta} onClick={() => signIn.mutate()}>
            <KeyRound /> Sign in again
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  )
}

/** adminTitle is what the repository pages say when a change needs a recent sign-in. */
const adminTitle = "Changing repository connections needs a recent sign-in"

/**
 * StepUpRefusal shows an action the server refused for want of a recent sign-in: sign in again,
 * which clears the refusal (reset), then try again.
 */
export function StepUpRefusal({ error, reset, title = adminTitle }: { error: unknown; reset?: () => void; title?: string }) {
  return <StepUpPrompt title={title} reason={problemMessage(error)} onSignedIn={reset} />
}

/**
 * AdminStepUp asks for a recent sign-in on the pages that change repository connections, when
 * auth.stepUp.admin is set and the person's sign-in is too old: before they fill a form rather
 * than when they save it.
 */
export function AdminStepUp() {
  const { data: me } = useQuery(getMeOptions())
  if (!me?.stepUp.admin || me.stepUp.satisfied) return null
  return <StepUpPrompt title={adminTitle} reason={me.stepUp.reason} />
}
