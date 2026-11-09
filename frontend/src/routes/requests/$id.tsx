import { useEffect, useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { AlertCircle, BellOff, Check, GitPullRequest, KeyRound, MessageSquare, RotateCcw, UserX, X } from "lucide-react"
import {
  cancelRequestMutation,
  getMeOptions,
  getMeQueryKey,
  getRequestOptions,
  getRequestQueryKey,
  listRequestsQueryKey,
  retryRequestMutation,
  reviewRequestMutation,
} from "@/api/generated/@tanstack/react-query.gen"
import type { DeliveryView, RequestView } from "@/api/generated/types.gen"
import { ApprovalChain } from "@/components/approval/approval-chain"
import { Codeowners } from "@/components/approval/codeowners"
import { Manager } from "@/components/approval/manager"
import { Separation } from "@/components/approval/separation"
import { ChangesView } from "@/components/diff/changes-view"
import { Plans } from "@/components/requests/plans"
import { StateBadge } from "@/components/requests/state-badge"
import { StepUpPrompt } from "@/components/step-up-prompt"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { chatChannels, chatLinkOf, useChatLink } from "@/lib/chat-link"
import { ago } from "@/lib/format"
import { fieldErrors, hasFieldErrors, isStepUpRequired, problemMessage } from "@/lib/problems"
import { eventText, inProgress, requestTitle, runURL } from "@/lib/requests"

// link is the link of an approval card in chat (Microsoft Teams) that opened the page.
type Search = { link?: string }

export const Route = createFileRoute("/requests/$id")({
  validateSearch: (s: Record<string, unknown>): Search => ({ link: chatLinkOf(s.link) }),
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getRequestOptions({ path: params })),
  component: RequestPage,
})

function RequestPage() {
  const { id } = Route.useParams()
  const { link } = Route.useSearch()
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
        {r.delivery && <DeliveryLine delivery={r.delivery} />}
        {r.requesterNotNotified && (
          <p className="text-muted-foreground flex items-center gap-1 text-sm">
            <BellOff className="size-4 shrink-0" aria-hidden />
            {r.requester} gets no notifications of this request: Quai knows no address of theirs on the channels it notifies on (a
            verified email; an Entra ID object ID for Microsoft Teams).
          </p>
        )}
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
          {(r.plans ?? []).length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle>Plan</CardTitle>
              </CardHeader>
              <CardContent>
                <Plans plans={r.plans ?? []} />
              </CardContent>
            </Card>
          )}
        </div>
        <div className="grid gap-6">
          <Card>
            <CardHeader>
              <CardTitle>Approval</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5">
              <ApprovalChain rules={r.rules ?? []} />
              {r.codeowners && <Codeowners codeowners={r.codeowners} />}
              {r.manager && <Manager manager={r.manager} submitted />}
              {r.separation && <Separation separation={r.separation} submitted />}
              {(r.reviews ?? []).length > 0 && (
                <ul className="grid gap-3" aria-label="Reviews">
                  {(r.reviews ?? []).map((v) => (
                    <li key={`${v.reviewer}-${v.at}`} className="flex items-start gap-2 text-sm">
                      {v.decision === "approve" ? <Check className="mt-0.5 size-4 text-emerald-600" aria-hidden /> : <X className="text-destructive mt-0.5 size-4" aria-hidden />}
                      <div>
                        <span className="font-medium">{v.reviewer}</span> {v.decision === "approve" ? "approved" : "denied"}{" "}
                        <span className="text-muted-foreground text-xs">
                          {ago(v.at)}
                          {v.stepUp && " · signed in recently"}
                        </span>
                        {v.comment && <p className="whitespace-pre-wrap">{v.comment}</p>}
                      </div>
                    </li>
                  ))}
                </ul>
              )}
              {r.canReview ? (
                <ReviewBox request={r} link={link} />
              ) : link ? (
                <ChatLinkNotice request={r} link={link} />
              ) : (
                r.cannotReview && <CannotReview reason={r.cannotReview} />
              )}
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
                    {runURL(e) && (
                      <>
                        {" "}
                        <a href={runURL(e)} target="_blank" rel="noreferrer" className="text-muted-foreground text-xs underline">
                          run
                        </a>
                      </>
                    )}
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
 * you have approved, the request waiting for others, you can still deny it. A request whose
 * approval asks for a step-up takes reviews only from people who signed in recently: until then
 * the box asks them to sign in again, and comes back here (the server checks it either way).
 *
 * Opened from the Approve or Deny of an approval card in chat (link), it offers that decision to
 * confirm; the review carries the link, which the server checks again. A link the server refuses
 * says why, and the box is as without it.
 */
function ReviewBox({ request, link }: { request: RequestView; link?: string }) {
  const [comment, setComment] = useState("")
  const queryClient = useQueryClient()
  const navigate = Route.useNavigate()
  const { data: me } = useQuery(getMeOptions())
  const chat = useChatLink(request.id, link)
  const update = useRequestUpdate()
  const review = useMutation({
    ...reviewRequestMutation(),
    onSuccess: async (next) => {
      await update(next)
      // Used: the page no longer offers its decision.
      if (link) void navigate({ search: {}, replace: true })
    },
    // Refused for want of a recent sign-in: how recently the person signed in is read again, and
    // the box asks them to sign in again.
    onError: (e) => {
      if (isStepUpRequired(e)) void queryClient.invalidateQueries({ queryKey: getMeQueryKey() })
    },
  })
  const approved = (request.reviews ?? []).filter((v) => v.reviewer === me?.name).at(-1)?.decision === "approve"
  const ready = chat.data && !(approved && chat.data.decision === "approve") ? chat.data : undefined
  const send = (decision: "approve" | "deny") =>
    review.mutate({ path: { id: request.id }, body: { decision, comment: comment || undefined, link: ready?.decision === decision ? link : undefined } })
  const error = fieldErrors(review.error, "body").comment
  const notice = link && <ChatLinkStatus chat={chat} />
  if (request.stepUp && me && !me.stepUp.satisfied)
    return (
      <div className="grid gap-3 border-t pt-4">
        {notice}
        <StepUpPrompt title="Reviewing this request needs a recent sign-in" reason={me.stepUp.reason} onSignedIn={() => review.reset()} />
      </div>
    )
  if (ready)
    return (
      <div className="grid gap-3 border-t pt-4">
        {notice}
        <Label htmlFor="review-comment">{ready.decision === "approve" ? "Comment" : "Why you deny it"}</Label>
        <Textarea
          id="review-comment"
          rows={3}
          maxLength={2000}
          placeholder={ready.decision === "approve" ? "Optional" : "Required to deny: say what to change"}
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
        <div className="flex flex-wrap gap-2">
          {ready.decision === "approve" ? (
            <Button type="button" disabled={review.isPending} onClick={() => send("approve")}>
              <Check /> Confirm approval
            </Button>
          ) : (
            <Button type="button" variant="destructive" disabled={review.isPending} onClick={() => send("deny")}>
              <X /> Confirm denial
            </Button>
          )}
          <Button type="button" variant="ghost" disabled={review.isPending} onClick={() => void navigate({ search: {}, replace: true })}>
            Review it otherwise
          </Button>
        </div>
      </div>
    )
  return (
    <div className="grid gap-3 border-t pt-4">
      {notice}
      {request.stepUp && me?.stepUp.until && (
        <p className="text-muted-foreground flex items-center gap-1.5 text-xs">
          <KeyRound className="size-3.5" aria-hidden />
          This request needs a recent sign-in: yours counts until {new Date(me.stepUp.until).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}.
        </p>
      )}
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

/** ChatLinkStatus says what the link of a chat card opened: a decision to confirm, or why it is refused. */
function ChatLinkStatus({ chat }: { chat: ReturnType<typeof useChatLink> }) {
  if (chat.isPending) return null
  if (chat.isError)
    return (
      <Alert role="status">
        <MessageSquare />
        <AlertTitle>This link from chat cannot be used</AlertTitle>
        <AlertDescription>{problemMessage(chat.error)}</AlertDescription>
      </Alert>
    )
  const { decision, channel } = chat.data
  return (
    <Alert role="status">
      <MessageSquare />
      <AlertTitle>
        {decision === "approve" ? "Approve" : "Deny"} from your {chatChannels[channel] ?? channel} card
      </AlertTitle>
      <AlertDescription>Read the request, then confirm your decision here: nothing is decided until you do.</AlertDescription>
    </Alert>
  )
}

/** ChatLinkNotice says why the link of a chat card does not open a decision to someone who cannot review. */
function ChatLinkNotice({ request, link }: { request: RequestView; link: string }) {
  const chat = useChatLink(request.id, link)
  if (chat.isPending) return null
  return (
    <div className="grid gap-3 border-t pt-4">
      {chat.isError ? (
        <ChatLinkStatus chat={chat} />
      ) : (
        <Alert role="status">
          <MessageSquare />
          <AlertTitle>This link from chat cannot be used</AlertTitle>
          <AlertDescription>{request.cannotReview || "You are not among the approvers of this request now."}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

/** CannotReview says why someone a rule names cannot review the request: they requested it, or changed its definitions. */
function CannotReview({ reason }: { reason: string }) {
  return (
    <div className="grid gap-3 border-t pt-4">
      <Alert role="status">
        <UserX />
        <AlertTitle>You cannot review this request</AlertTitle>
        <AlertDescription>{reason}</AlertDescription>
      </Alert>
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

/** DeliveryLine links the request's pull request, or says it is being opened. */
function DeliveryLine({ delivery }: { delivery: DeliveryView }) {
  return (
    <p className="text-muted-foreground flex flex-wrap items-center gap-1 text-sm">
      <GitPullRequest className="size-4" aria-hidden />
      {delivery.url ? (
        <a href={delivery.url} target="_blank" rel="noreferrer" className="text-foreground underline-offset-4 hover:underline">
          Pull request #{delivery.pullRequest}
        </a>
      ) : (
        <span>Opening the pull request</span>
      )}{" "}
      from <code className="font-mono">{delivery.branch}</code>
      {delivery.mergeCommit && (
        <>
          {" "}
          · merged as <code className="font-mono">{delivery.mergeCommit.slice(0, 12)}</code>
        </>
      )}
    </p>
  )
}
