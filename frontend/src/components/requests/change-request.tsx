import { AlertTriangle, ClipboardCheck, ExternalLink } from "lucide-react"
import type { ChangeRequestView, RequestView } from "@/api/generated/types.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { kindText, outcomeText, readText, supersededText } from "@/lib/change-request"
import { ago } from "@/lib/format"

/**
 * ChangeRequestPanel shows the request's change request in ServiceNow, which an
 * external: servicenow rule waits for (docs/design/05-delivery-and-approvals.md#itsm): its number
 * and link, what ServiceNow reported last, what it means for the request, and why Quai cannot
 * reach ServiceNow or what ServiceNow refused. What ServiceNow says is shown as text.
 */
export function ChangeRequestPanel({ request: r, change: c }: { request: RequestView; change: ChangeRequestView }) {
  const waiting = (r.state === "pending_approval" || r.state === "approved") && !r.breakGlass
  const read = readText(c)
  return (
    <section className="grid gap-2 rounded-md border p-3 text-sm" aria-label="ServiceNow change request" data-testid="change-request">
      <div className="flex flex-wrap items-center gap-2">
        <ClipboardCheck className="text-muted-foreground size-4" aria-hidden />
        <span className="font-medium">ServiceNow</span>
        {c.number &&
          (c.url ? (
            <a href={c.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-mono underline-offset-4 hover:underline">
              {c.number}
              <ExternalLink className="size-3" aria-hidden />
            </a>
          ) : (
            <span className="font-mono">{c.number}</span>
          ))}
        {c.kind && <Badge variant="outline">{kindText(c)}</Badge>}
      </div>
      {read && (
        <p className="text-muted-foreground text-xs">
          {read}
          {c.polledAt && <> · read {ago(c.polledAt)}</>}
        </p>
      )}
      <p>{outcomeText(c, waiting)}</p>
      {c.failure && (
        <Alert>
          <AlertTriangle aria-hidden />
          <AlertTitle>Quai cannot reach ServiceNow{c.failingSince ? ` since ${ago(c.failingSince)}` : ""}</AlertTitle>
          <AlertDescription>
            <p>It tries again; the request waits, and nothing is approved meanwhile.</p>
            <p className="font-mono text-xs break-all">{c.failure}</p>
          </AlertDescription>
        </Alert>
      )}
      {c.problem && (
        <Alert variant="destructive">
          <AlertTriangle aria-hidden />
          <AlertTitle>{c.number ? "Someone must act in ServiceNow" : "The rule cannot be satisfied"}</AlertTitle>
          <AlertDescription>{c.problem}</AlertDescription>
        </Alert>
      )}
      {supersededText(c.superseded) && <p className="text-muted-foreground text-xs">{supersededText(c.superseded)}</p>}
    </section>
  )
}
