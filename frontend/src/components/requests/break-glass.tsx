import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertCircle, AlertTriangle, Check, ExternalLink, ShieldAlert } from "lucide-react"
import {
  breakGlassMutation,
  getMeOptions,
  getMeQueryKey,
  getRequestQueryKey,
  listBreakGlassQueryKey,
  listRequestsQueryKey,
  signOffBreakGlassMutation,
} from "@/api/generated/@tanstack/react-query.gen"
import type { BreakGlassView, RequestView } from "@/api/generated/types.gen"
import { StepUpPrompt } from "@/components/step-up-prompt"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { capacityText, incidentProblemTitle, incidentText, reviewStatus } from "@/lib/break-glass"
import { ago } from "@/lib/format"
import { fieldErrors, hasFieldErrors, isStepUpRequired, problemMessage } from "@/lib/problems"

// Break-glass (docs/design/05-delivery-and-approvals.md#break-glass): a member of the break-glass
// role merges a request without its approvals, and two people sign off its post-incident review.
// What people typed is shown as text, never as markup.

/** useBreakGlassUpdate stores a request a break-glass mutation returned and refreshes the lists it appears in. */
function useBreakGlassUpdate() {
  const queryClient = useQueryClient()
  return async (next: RequestView) => {
    queryClient.setQueryData(getRequestQueryKey({ path: { id: next.id } }), next)
    await queryClient.invalidateQueries({ queryKey: listRequestsQueryKey() })
    await queryClient.invalidateQueries({ queryKey: listBreakGlassQueryKey() })
  }
}

/**
 * BreakGlassAction offers a member of the break-glass role to merge the request without its
 * approvals, past its change windows, in a dialog that says what it does and asks why and for the
 * incident; with auth.stepUp configured, after signing in again. The break-glass carries the
 * content the page shows: the server refuses it if the request changed since.
 */
export function BreakGlassAction({ request }: { request: RequestView }) {
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState("")
  const [incident, setIncident] = useState("")
  const queryClient = useQueryClient()
  const { data: me } = useQuery(getMeOptions())
  const update = useBreakGlassUpdate()
  const breakGlass = useMutation({
    ...breakGlassMutation(),
    onSuccess: async (next) => {
      await update(next)
      setOpen(false)
    },
    onError: (e) => {
      if (isStepUpRequired(e)) void queryClient.invalidateQueries({ queryKey: getMeQueryKey() })
    },
  })
  if (!request.canBreakGlass) {
    if (!request.cannotBreakGlass) return null
    return (
      <p className="text-muted-foreground flex items-start gap-1.5 border-t pt-4 text-xs">
        <ShieldAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
        <span>You cannot break glass for this request: {request.cannotBreakGlass}</span>
      </p>
    )
  }
  const errors = fieldErrors(breakGlass.error, "body")
  const needsStepUp = request.breakGlassStepUp && me && !me.stepUp.satisfied
  const held = request.state === "approved"
  return (
    <div className="grid gap-2 border-t pt-4">
      <Dialog
        open={open}
        onOpenChange={(o) => {
          setOpen(o)
          if (!o) breakGlass.reset()
        }}
      >
        <DialogTrigger asChild>
          <Button type="button" variant="outline" className="border-destructive text-destructive hover:text-destructive justify-self-start">
            <ShieldAlert /> Break glass
          </Button>
        </DialogTrigger>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Break glass for request #{request.number}?</DialogTitle>
            <DialogDescription>
              {held ? "It merges now, past its change windows." : "It merges now, without its approvals and past its change windows."} The platform's own checks
              and branch protection still apply. Security and the owning team are alerted at once, and a post-incident review stays open until two reviewers
              sign it off.
            </DialogDescription>
          </DialogHeader>
          {needsStepUp ? (
            <StepUpPrompt title="Breaking glass needs a recent sign-in" reason={me.stepUp.reason} onSignedIn={() => breakGlass.reset()} />
          ) : (
            <form
              id="break-glass-form"
              className="grid gap-3"
              onSubmit={(e) => {
                e.preventDefault()
                breakGlass.mutate({
                  path: { id: request.id },
                  body: {
                    reason,
                    incident,
                    changeSet: request.changeSet ?? "",
                    head: request.delivery?.head || undefined,
                  },
                })
              }}
            >
              <div className="grid gap-1.5">
                <Label htmlFor="break-glass-incident">Incident</Label>
                <Input
                  id="break-glass-incident"
                  maxLength={64}
                  placeholder="INC0012345"
                  value={incident}
                  aria-invalid={errors.incident ? true : undefined}
                  onChange={(e) => setIncident(e.target.value)}
                />
                {errors.incident && <p className="text-destructive text-xs">{errors.incident}</p>}
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="break-glass-reason">Why it cannot wait for its approvals</Label>
                <Textarea
                  id="break-glass-reason"
                  rows={3}
                  maxLength={500}
                  value={reason}
                  aria-invalid={errors.reason ? true : undefined}
                  // One line, as the server takes it: a line break becomes a space.
                  onChange={(e) => setReason(e.target.value.replace(/\r?\n/g, " "))}
                />
                <p className="text-muted-foreground text-xs">Kept in Quai and the audit log; never sent to the Git platform or in the alerts.</p>
                {errors.reason && <p className="text-destructive text-xs">{errors.reason}</p>}
              </div>
              {breakGlass.isError && !hasFieldErrors(breakGlass.error) && (
                <Alert variant="destructive">
                  <AlertCircle />
                  <AlertDescription>{problemMessage(breakGlass.error)}</AlertDescription>
                </Alert>
              )}
            </form>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="ghost">
                Cancel
              </Button>
            </DialogClose>
            {!needsStepUp && (
              <Button type="submit" form="break-glass-form" variant="destructive" disabled={breakGlass.isPending}>
                <ShieldAlert /> Break glass and merge
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

/**
 * BreakGlassCard shows a request's break-glass, who and why, what ServiceNow said of its incident
 * when it is configured, and its post-incident review: what ServiceNow did not confirm, the
 * sign-offs so far, and the box to sign it off for those who may. The incident's link is the one
 * the server built from the configured instance, never one typed.
 */
export function BreakGlassCard({ request, breakGlass: bg }: { request: RequestView; breakGlass: BreakGlassView }) {
  return (
    <section aria-label="Break-glass" className="grid gap-3">
      <Alert variant="destructive">
        <AlertTriangle />
        <AlertTitle>
          Break-glass by {bg.by} {ago(bg.at)}
        </AlertTitle>
        <AlertDescription className="grid gap-1">
          <p>
            {bg.from === "approved" ? "Merged past its change windows" : "Merged without its approvals"}, for incident{" "}
            {bg.incidentUrl ? (
              <a href={bg.incidentUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-mono underline underline-offset-4">
                {bg.incident}
                <ExternalLink className="size-3" aria-hidden />
              </a>
            ) : (
              <code className="font-mono">{bg.incident}</code>
            )}
            .
          </p>
          {incidentText(bg) && <p>{incidentText(bg)}</p>}
          <p className="text-foreground whitespace-pre-wrap">{bg.reason}</p>
        </AlertDescription>
      </Alert>
      <div className="grid gap-2">
        <h3 className="text-sm font-medium">Post-incident review</h3>
        <p className="text-muted-foreground text-sm">{reviewStatus(bg)}</p>
        {bg.incidentProblem && (
          <Alert variant="destructive" data-testid="incident-problem">
            <AlertCircle />
            <AlertTitle>{incidentProblemTitle(bg)}</AlertTitle>
            <AlertDescription>{bg.incidentProblem}</AlertDescription>
          </Alert>
        )}
        {(bg.signOffs ?? []).length > 0 && (
          <ul className="grid gap-3" aria-label="Sign-offs">
            {(bg.signOffs ?? []).map((s) => (
              <li key={s.signer} className="flex items-start gap-2 text-sm">
                <Check className="mt-0.5 size-4 text-emerald-600" aria-hidden />
                <div>
                  <span className="font-medium">{s.signer}</span> signed it off as {capacityText(s.capacity)}{" "}
                  <span className="text-muted-foreground text-xs">
                    {ago(s.at)}
                    {s.stepUp && " · signed in recently"}
                  </span>
                  <p className="whitespace-pre-wrap">{s.comment}</p>
                </div>
              </li>
            ))}
          </ul>
        )}
        {bg.canSignOff ? (
          <SignOffBox request={request} />
        ) : (
          bg.open && bg.cannotSignOff && <p className="text-muted-foreground text-xs">You cannot sign it off: {bg.cannotSignOff}</p>
        )}
      </div>
    </section>
  )
}

/** SignOffBox signs off a break-glass review, with what the review found; never withdrawn. */
function SignOffBox({ request }: { request: RequestView }) {
  const [comment, setComment] = useState("")
  const queryClient = useQueryClient()
  const { data: me } = useQuery(getMeOptions())
  const update = useBreakGlassUpdate()
  const signOff = useMutation({
    ...signOffBreakGlassMutation(),
    onSuccess: update,
    onError: (e) => {
      if (isStepUpRequired(e)) void queryClient.invalidateQueries({ queryKey: getMeQueryKey() })
    },
  })
  if (request.breakGlassStepUp && me && !me.stepUp.satisfied)
    return <StepUpPrompt title="Signing off a break-glass review needs a recent sign-in" reason={me.stepUp.reason} onSignedIn={() => signOff.reset()} />
  const error = fieldErrors(signOff.error, "body").comment
  return (
    <div className="grid gap-2">
      <Label htmlFor="sign-off-comment">What the review found</Label>
      <Textarea
        id="sign-off-comment"
        rows={3}
        maxLength={2000}
        value={comment}
        aria-invalid={error ? true : undefined}
        onChange={(e) => setComment(e.target.value)}
      />
      {error && <p className="text-destructive text-xs">{error}</p>}
      {signOff.isError && !hasFieldErrors(signOff.error) && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertDescription>{problemMessage(signOff.error)}</AlertDescription>
        </Alert>
      )}
      <p className="text-muted-foreground text-xs">A sign-off cannot be withdrawn.</p>
      <Button
        type="button"
        className="justify-self-start"
        disabled={signOff.isPending}
        onClick={() => signOff.mutate({ path: { id: request.id }, body: { comment } })}
      >
        <Check /> Sign off the review
      </Button>
    </div>
  )
}
