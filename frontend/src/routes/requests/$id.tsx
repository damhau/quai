import { useEffect, useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { AlertCircle, Check, RotateCcw, X } from "lucide-react"
import {
  cancelRequestMutation,
  getMeOptions,
  getRequestOptions,
  getRequestQueryKey,
  listRequestsQueryKey,
  retryRequestMutation,
  reviewRequestMutation,
} from "@/api/generated/@tanstack/react-query.gen"
import type { RequestView } from "@/api/generated/types.gen"
import { ApprovalChain } from "@/components/approval/approval-chain"
import { ChangesView } from "@/components/diff/changes-view"
import { StateBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { ago } from "@/lib/format"
import { fieldErrors, hasFieldErrors, problemMessage } from "@/lib/problems"
import { eventText, inProgress, requestTitle } from "@/lib/requests"

export const Route = createFileRoute("/requests/$id")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getRequestOptions({ path: params })),
  component: RequestPage,
})

function RequestPage() {
  const { id } = Route.useParams()
  const queryClient = useQueryClient()
  const { data: r } = useSuspenseQuery({
    ...getRequestOptions({ path: { id } }),
    refetchInterval: (q) => (q.state.data && inProgress(q.state.data.state) ? 4000 : false),
  })
  // Once applied, the inventory, the resources and the log have changed: what is cached of them
  // is stale.
  const applied = r.state === "applied"
  useEffect(() => {
    if (applied) void queryClient.invalidateQueries({ predicate: (q) => (q.queryKey[0] as { _id?: string } | undefined)?._id !== "getRequest" })
  }, [applied, queryClient])
  return (
    <div className="grid gap-6">
      <header className="grid gap-1">
        <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
          <Link to="/requests" search={{ view: "mine" }} className="hover:text-foreground">
            Requests
          </Link>{" "}
          / #{r.number}
        </nav>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-2xl font-semibold tracking-tight">
            #{r.number} {requestTitle(r)}
          </h1>
          <StateBadge state={r.state} />
        </div>
        <p className="text-muted-foreground text-sm">
          Requested by <span className="text-foreground">{r.requester}</span> {ago(r.createdAt)}
          {r.stack && (
            <>
              {" "}
              · applied by the stack <code className="font-mono">{r.stack}</code>
            </>
          )}
        </p>
      </header>
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <div className="grid gap-6">
          <Card>
            <CardHeader>
              <CardTitle>Reason</CardTitle>
            </CardHeader>
            <CardContent>{r.reason ? <p className="whitespace-pre-wrap">{r.reason}</p> : <p className="text-muted-foreground">No reason given.</p>}</CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Changes</CardTitle>
            </CardHeader>
            <CardContent>
              <ChangesView changes={r.changes ?? []} />
            </CardContent>
          </Card>
        </div>
        <div className="grid gap-6">
          <Card>
            <CardHeader>
              <CardTitle>Approval</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5">
              <ApprovalChain rules={r.rules ?? []} />
              {(r.reviews ?? []).length > 0 && (
                <ul className="grid gap-3" aria-label="Reviews">
                  {(r.reviews ?? []).map((v) => (
                    <li key={`${v.reviewer}-${v.at}`} className="flex items-start gap-2 text-sm">
                      {v.decision === "approve" ? <Check className="mt-0.5 size-4 text-emerald-600" aria-hidden /> : <X className="text-destructive mt-0.5 size-4" aria-hidden />}
                      <div>
                        <span className="font-medium">{v.reviewer}</span> {v.decision === "approve" ? "approved" : "denied"}{" "}
                        <span className="text-muted-foreground text-xs">{ago(v.at)}</span>
                        {v.comment && <p className="whitespace-pre-wrap">{v.comment}</p>}
                      </div>
                    </li>
                  ))}
                </ul>
              )}
              {r.canReview && <ReviewBox request={r} />}
              <Actions request={r} />
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Timeline</CardTitle>
            </CardHeader>
            <CardContent>
              <ol className="border-muted grid gap-3 border-l pl-4" aria-label="Timeline">
                {(r.events ?? []).map((e, i) => (
                  <li key={i} className="text-sm">
                    {e.actor && <span className="font-medium">{e.actor} </span>}
                    {eventText(e)}
                    <div className="text-muted-foreground text-xs" title={new Date(e.at).toLocaleString()}>
                      {ago(e.at)}
                    </div>
                  </li>
                ))}
              </ol>
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

/** useRequestUpdate stores a request a mutation returned and refreshes the lists it appears in. */
function useRequestUpdate() {
  const queryClient = useQueryClient()
  return async (next: RequestView) => {
    queryClient.setQueryData(getRequestQueryKey({ path: { id: next.id } }), next)
    await queryClient.invalidateQueries({ queryKey: listRequestsQueryKey() })
  }
}

/**
 * ReviewBox lets an approver decide. Each reviewer's latest decision counts (design 05), so once
 * you have approved, the request waiting for others, you can still deny it.
 */
function ReviewBox({ request }: { request: RequestView }) {
  const [comment, setComment] = useState("")
  const { data: me } = useQuery(getMeOptions())
  const update = useRequestUpdate()
  const review = useMutation({ ...reviewRequestMutation(), onSuccess: update })
  const send = (decision: "approve" | "deny") => review.mutate({ path: { id: request.id }, body: { decision, comment: comment || undefined } })
  const error = fieldErrors(review.error, "body").comment
  const approved = (request.reviews ?? []).filter((v) => v.reviewer === me?.name).at(-1)?.decision === "approve"
  return (
    <div className="grid gap-3 border-t pt-4">
      {approved && <p className="text-sm">You approved this request; it waits for the other approvals. You can still deny it.</p>}
      <Label htmlFor="review-comment">{approved ? "Change your decision" : "Your review"}</Label>
      <Textarea
        id="review-comment"
        rows={3}
        maxLength={2000}
        placeholder="Comment; required to deny: say what to change"
        value={comment}
        aria-invalid={error ? true : undefined}
        onChange={(e) => setComment(e.target.value)}
      />
      {error && <p className="text-destructive text-xs">{error}</p>}
      {review.isError && !hasFieldErrors(review.error) && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertDescription>{problemMessage(review.error)}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        {!approved && (
          <Button type="button" disabled={review.isPending} onClick={() => send("approve")}>
            <Check /> Approve
          </Button>
        )}
        <Button type="button" variant="destructive" disabled={review.isPending} onClick={() => send("deny")}>
          <X /> Deny
        </Button>
      </div>
    </div>
  )
}

function Actions({ request }: { request: RequestView }) {
  const update = useRequestUpdate()
  const cancel = useMutation({ ...cancelRequestMutation(), onSuccess: update })
  const retry = useMutation({ ...retryRequestMutation(), onSuccess: update })
  if (!request.canCancel && !request.canRetry) return null
  const error = cancel.error ?? retry.error
  return (
    <div className="grid gap-3 border-t pt-4">
      {error && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        {request.canRetry && (
          <Button type="button" variant="outline" disabled={retry.isPending} onClick={() => retry.mutate({ path: { id: request.id } })}>
            <RotateCcw /> Retry delivery
          </Button>
        )}
        {request.canCancel && (
          <Button type="button" variant="ghost" disabled={cancel.isPending} onClick={() => cancel.mutate({ path: { id: request.id } })}>
            Cancel request
          </Button>
        )}
      </div>
    </div>
  )
}
