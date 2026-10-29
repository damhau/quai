import { Link } from "@tanstack/react-router"
import { AlertTriangle, ExternalLink } from "lucide-react"
import type { PlanChange, RunView } from "@/api/generated/types.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { ago, webURL } from "@/lib/format"
import { resourceParams } from "@/lib/resources"

const destructive = (c: PlanChange) => c.action === "delete" || c.action === "replace"

/** destroyed lists the Terraform resources the plans delete or replace. */
function destroyed(plans: RunView[]): PlanChange[] {
  return plans.flatMap((p) => [...(p.plan?.resources ?? []).flatMap((r) => r.changes ?? []), ...(p.plan?.unmapped ?? [])]).filter(destructive)
}

/** Plans shows the latest plan of each stack a request's pull request was planned in: what it
 *  changes, by resource, its policies and its cost; what it destroys comes first. */
export function Plans({ plans }: { plans: RunView[] }) {
  const destroys = destroyed(plans)
  return (
    <div className="grid gap-5">
      {destroys.length > 0 && (
        <Alert variant="destructive">
          <AlertTriangle />
          <AlertTitle>
            The plan destroys {destroys.length} Terraform resource{destroys.length === 1 ? "" : "s"}
          </AlertTitle>
          <AlertDescription>
            <ul className="font-mono text-xs">
              {destroys.map((c) => (
                <li key={c.address}>
                  {c.address} {c.action === "replace" && "(replaced)"}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {plans.map((p) => (
        <section key={p.id} className="grid gap-3" aria-label={`Plan of ${p.stack}`}>
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <code className="font-mono">{p.stack}</code>
            <Badge variant={p.status === "failed" ? "destructive" : "outline"}>{p.status === "running" ? "planning" : p.status}</Badge>
            {p.summary && (
              <span aria-label="Summary">
                +{p.summary.add} ~{p.summary.change} <span className={p.summary.destroy > 0 ? "text-destructive font-medium" : ""}>−{p.summary.destroy}</span>
              </span>
            )}
            {p.cost && (
              <span>
                {p.cost.monthlyDelta >= 0 ? "+" : "−"}
                {p.cost.currency} {Math.abs(p.cost.monthlyDelta).toLocaleString()}/month
              </span>
            )}
            <span className="text-muted-foreground text-xs">{ago(p.finishedAt ?? p.startedAt)}</span>
            {webURL(p.url) && (
              <a href={webURL(p.url)} target="_blank" rel="noreferrer" className="text-muted-foreground inline-flex items-center gap-1 text-xs underline">
                run <ExternalLink className="size-3" aria-hidden />
              </a>
            )}
          </div>
          {(p.policies ?? []).length > 0 && (
            <ul className="flex flex-wrap gap-1" aria-label="Policies">
              {(p.policies ?? []).map((pol) => (
                <li key={pol.name}>
                  <Badge variant={pol.result === "failed" ? "destructive" : pol.result === "warning" ? "secondary" : "outline"} title={pol.message}>
                    {pol.name}: {pol.result}
                  </Badge>
                </li>
              ))}
            </ul>
          )}
          {p.plan && (
            <ul className="grid gap-2 text-sm">
              {(p.plan.resources ?? []).map((r) => (
                <li key={r.resource}>
                  <Link to="/resources/$repository/$type/$" params={resourceParams(r.resource)} className="font-medium hover:underline">
                    {r.key}
                  </Link>{" "}
                  <span className="text-muted-foreground text-xs">{r.type}</span>
                  <Changes changes={r.changes ?? []} />
                </li>
              ))}
              {(p.plan.unmapped ?? []).length > 0 && (
                <li>
                  <span className="font-medium">Not described by a type</span>
                  <Changes changes={p.plan.unmapped ?? []} />
                </li>
              )}
              {(p.plan.ambiguous ?? []).map((a) => (
                <li key={a.address} className="text-muted-foreground text-xs">
                  <code className="font-mono">{a.address}</code> belongs to several resources: {(a.resources ?? []).join(", ")}
                </li>
              ))}
            </ul>
          )}
        </section>
      ))}
    </div>
  )
}

function Changes({ changes }: { changes: PlanChange[] }) {
  return (
    <ul className="mt-1 grid gap-1">
      {changes.map((c) => (
        <li key={c.address} className="grid gap-0.5 text-xs">
          <span className="flex flex-wrap items-center gap-2">
            <Badge variant={destructive(c) ? "destructive" : "outline"}>{c.action}</Badge>
            <code className="font-mono">{c.address}</code>
          </span>
          {(c.attributes ?? []).length > 0 && (
            <span className="text-muted-foreground pl-1">
              {(c.attributes ?? []).map((a, i) => {
                const v = c.values?.[a]
                return (
                  <span key={a}>
                    {i > 0 && ", "}
                    <code className="font-mono">{a}</code>
                    {v && (v.unknown ? `: ${show(v.before)} → known after apply` : `: ${show(v.before)} → ${show(v.after)}`)}
                  </span>
                )
              })}
            </span>
          )}
        </li>
      ))}
    </ul>
  )
}

function show(v: unknown): string {
  if (v === undefined || v === null) return "none"
  return typeof v === "string" ? v : JSON.stringify(v)
}
