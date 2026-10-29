import { useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Clock, Zap } from "lucide-react"
import { getBlueprintOptions, getInstanceOptions } from "@/api/generated/@tanstack/react-query.gen"
import type { ActionView } from "@/api/generated/types.gen"
import { InstanceActionDialog } from "@/components/day2/instance-action-dialog"
import { ResourceStateBadge, ResourceStatusBadge, StateBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ago } from "@/lib/format"
import { openStates, resourceParams } from "@/lib/resources"

export const Route = createFileRoute("/deployments/$repository/$blueprint/$name")({
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(getInstanceOptions({ path: params })),
      context.queryClient.ensureQueryData(getBlueprintOptions({ path: { repository: params.repository, name: params.blueprint } })),
    ]),
  component: DeploymentPage,
})

const show = (v: unknown) => (typeof v === "string" ? v : JSON.stringify(v))

function DeploymentPage() {
  const params = Route.useParams()
  const { data: inst } = useSuspenseQuery({ ...getInstanceOptions({ path: params }), refetchInterval: 15_000 })
  const { data: b } = useSuspenseQuery(getBlueprintOptions({ path: { repository: params.repository, name: params.blueprint } }))
  const [action, setAction] = useState<ActionView | null>(null)
  const requests = inst.requests ?? []
  const open = requests.find((q) => openStates.includes(q.state))
  const resources = inst.resources ?? []
  return (
    <div className="grid gap-6">
      <header className="grid gap-1">
        <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
          <Link to="/inventory" className="hover:text-foreground">
            Inventory
          </Link>{" "}
          / {b.title} / {inst.name}
        </nav>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-2xl font-semibold tracking-tight">{inst.name}</h1>
          <Badge variant="secondary">{b.title}</Badge>
          {inst.blueprintVersion && <span className="text-muted-foreground text-xs">v{inst.blueprintVersion}</span>}
        </div>
        <p className="text-muted-foreground text-sm">
          {inst.owner ? <>owned by {inst.owner}</> : "no owner on record"} · <code className="font-mono">{inst.path}</code>
        </p>
      </header>
      {open && (
        <Alert>
          <Clock />
          <AlertDescription>
            <span>
              <Link to="/requests/$id" params={{ id: open.id }} className="underline">
                #{open.number}
              </Link>{" "}
              is in progress; new actions wait until it completes.
            </span>
          </AlertDescription>
        </Alert>
      )}
      {inst.canOperate && (b.actions ?? []).length > 0 && (
        <div className="flex flex-wrap gap-2" role="toolbar" aria-label="Day-2 actions">
          {(b.actions ?? []).map((a) => (
            <Button key={a.name} variant="outline" disabled={!!open} title={a.description} onClick={() => setAction(a)}>
              <Zap /> {a.title ?? a.name}
            </Button>
          ))}
        </div>
      )}
      <div className="grid items-start gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Resources</CardTitle>
          </CardHeader>
          <CardContent>
            <ul className="grid gap-2 text-sm">
              {resources.map((r) => (
                <li key={r.id} className="flex flex-wrap items-center gap-2">
                  <span className="text-muted-foreground w-16 shrink-0">{r.id}</span>
                  {r.exists ? (
                    <Link to="/resources/$repository/$type/$" params={resourceParams(r.resource)} className="font-mono text-xs hover:underline">
                      {r.resource}
                    </Link>
                  ) : (
                    <code className="font-mono text-xs">{r.resource}</code>
                  )}
                  {r.exists ? <ResourceStatusBadge status={r.status ?? "conforming"} /> : <Badge variant="destructive">missing</Badge>}
                  {r.exists && r.state && <ResourceStateBadge state={r.state} />}
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
        <div className="grid gap-6">
          <Card>
            <CardHeader>
              <CardTitle>Inputs</CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                {Object.entries(inst.inputs ?? {}).map(([k, v]) => (
                  <div key={k} className="contents">
                    <dt className="text-muted-foreground">{k}</dt>
                    <dd className="font-mono text-xs leading-5">{show(v)}</dd>
                  </div>
                ))}
              </dl>
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Requests</CardTitle>
            </CardHeader>
            <CardContent>
              <ul className="grid gap-2 text-sm">
                {requests.map((q) => (
                  <li key={q.id} className="flex flex-wrap items-center gap-2">
                    <Link to="/requests/$id" params={{ id: q.id }} className="hover:underline">
                      #{q.number} {q.kind === "action" ? q.action : q.kind}
                    </Link>
                    <StateBadge state={q.state} />
                    <span className="text-muted-foreground text-xs">
                      {q.requester}, {ago(q.createdAt)}
                    </span>
                  </li>
                ))}
              </ul>
            </CardContent>
          </Card>
        </div>
      </div>
      <InstanceActionDialog instance={inst} action={action} onClose={() => setAction(null)} />
    </div>
  )
}
