import { useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { AlertCircle, Clock, Pencil, ShieldAlert, Trash2, Zap } from "lucide-react"
import { getResourceOptions, getTypeOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Day2Dialog, type Day2 } from "@/components/day2/day2-dialog"
import { ResourceStatusBadge, StateBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ago, shortSha } from "@/lib/format"
import { requestTitle } from "@/lib/requests"
import { openStates } from "@/lib/resources"

export const Route = createFileRoute("/resources/$repository/$type/$")({
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(getResourceOptions({ query: { id: `${params.repository}/${params.type}/${params._splat}` } })),
      context.queryClient.ensureQueryData(getTypeOptions({ path: { repository: params.repository, name: params.type } })),
    ]),
  component: ResourcePage,
})

function ResourcePage() {
  const { repository, type: typeName, _splat: key } = Route.useParams()
  const id = `${repository}/${typeName}/${key}`
  const { data: r } = useSuspenseQuery({ ...getResourceOptions({ query: { id } }), refetchInterval: 15_000 })
  const { data: t } = useSuspenseQuery(getTypeOptions({ path: { repository, name: typeName } }))
  const [day2, setDay2] = useState<Day2 | null>(null)
  const requests = r.requests ?? []
  const open = requests.find((q) => openStates.includes(q.state))
  const changeable = r.canOperate && r.status === "conforming" && !open
  return (
    <div className="grid gap-6">
      <header className="grid gap-1">
        <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
          <Link to="/inventory" className="hover:text-foreground">
            Inventory
          </Link>{" "}
          / {t.title} / {r.key}
        </nav>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-2xl font-semibold tracking-tight">{r.params.name ?? r.key}</h1>
          {Object.entries(r.params)
            .filter(([k]) => k !== "name")
            .map(([k, v]) => (
              <Badge key={k} variant="secondary" title={k}>
                {v}
              </Badge>
            ))}
          <ResourceStatusBadge status={r.status} />
        </div>
        <p className="text-muted-foreground text-sm">
          {t.title} · {r.owner ? <>owned by {r.owner}</> : "no owner on record"} · <code className="font-mono">{r.path}</code>
          {r.stack && (
            <>
              {" "}
              · stack <code className="font-mono">{r.stack}</code>
            </>
          )}
        </p>
      </header>

      {r.error && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>Quai cannot read this file, so it will not edit it</AlertTitle>
          <AlertDescription>{r.error}</AlertDescription>
        </Alert>
      )}
      {r.status === "non-conforming" && (
        <Alert>
          <ShieldAlert />
          <AlertTitle>The file does not match its schema; fix it in Git before Quai edits it</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-4">
              {Object.entries(r.problems ?? {}).map(([f, m]) => (
                <li key={f}>
                  <code className="font-mono">{f}</code> {m}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {open && (
        <Alert>
          <Clock />
          <AlertDescription>
            <span>
              <Link to="/requests/$id" params={{ id: open.id }} className="underline">
                #{open.number} {requestTitle({ kind: open.kind, action: open.action, target: r.key })}
              </Link>{" "}
              is in progress; new changes wait until it completes.
            </span>
          </AlertDescription>
        </Alert>
      )}
      {!r.canOperate && !r.error && (
        <p className="text-muted-foreground text-sm">{r.owner ? `Only ${r.owner} or an operator can change this resource.` : "Only an operator can change a resource without an owner."}</p>
      )}
      {r.canOperate && (
        <div className="flex flex-wrap gap-2" role="toolbar" aria-label="Day-2 actions">
          <Button variant="outline" disabled={!changeable} onClick={() => setDay2({ kind: "update" })}>
            <Pencil /> Edit
          </Button>
          {(r.actions ?? []).map((a) => (
            <Button
              key={a.name}
              variant="outline"
              disabled={!changeable || a.kind === "imperative"}
              title={a.kind === "imperative" ? "Runs through an executor: not available yet" : a.description}
              onClick={() => setDay2({ kind: "action", action: a })}
            >
              <Zap /> {a.title ?? a.name}
            </Button>
          ))}
          {t.deletion !== "forbidden" && (
            <Button variant="outline" className="text-destructive" disabled={!changeable} onClick={() => setDay2({ kind: "delete" })}>
              <Trash2 /> Delete
            </Button>
          )}
        </div>
      )}

      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <Card>
          <CardHeader>
            <CardTitle>File</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="bg-muted/40 overflow-x-auto rounded-md p-3 font-mono text-xs leading-5" aria-label={`Content of ${r.path}`}>
              {r.raw}
            </pre>
          </CardContent>
        </Card>
        <div className="grid gap-6">
          <Card>
            <CardHeader>
              <CardTitle>Last apply</CardTitle>
            </CardHeader>
            <CardContent className="text-sm">
              {r.lastApply ? (
                <p>
                  <Badge variant="outline">{r.lastApply.status}</Badge> {r.lastApply.kind} of {shortSha(r.lastApply.commit)} {ago(r.lastApply.startedAt)}
                  {r.lastApply.url && (
                    <>
                      {" "}
                      ·{" "}
                      <a href={r.lastApply.url} className="underline" rel="noreferrer" target="_blank">
                        run
                      </a>
                    </>
                  )}
                </p>
              ) : (
                <p className="text-muted-foreground">No run reported for this stack yet.</p>
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Requests</CardTitle>
            </CardHeader>
            <CardContent>
              {requests.length ? (
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
              ) : (
                <p className="text-muted-foreground text-sm">No requests through Quai yet: this resource was found in the repository.</p>
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Git history</CardTitle>
            </CardHeader>
            <CardContent>
              <ol className="grid gap-2 text-sm">
                {(r.history ?? []).map((c) => (
                  <li key={c.sha}>
                    <code className="text-muted-foreground font-mono text-xs">{shortSha(c.sha)}</code> {c.subject}
                    <div className="text-muted-foreground text-xs">
                      {c.author}, {ago(c.date)}
                    </div>
                  </li>
                ))}
              </ol>
            </CardContent>
          </Card>
        </div>
      </div>
      <Day2Dialog resource={r} type={t} day2={day2} onClose={() => setDay2(null)} />
    </div>
  )
}
