import type { ReactNode } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useMutation, useQuery } from "@tanstack/react-query"
import { Loader2, Pencil, ScanSearch } from "lucide-react"
import { getRepositoryOptions, repositoryHealthOptions, testRepositoryAccessMutation } from "@/api/generated/@tanstack/react-query.gen"
import type { HealthView } from "@/api/generated/types.gen"
import { AccessReport, EnforcementResult, InstallWebhook } from "@/components/repositories/checks"
import { Problems } from "@/components/repositories/problems"
import { StackRuns } from "@/components/runs/stack-runs"
import { TrustWarning } from "@/components/trust-warning"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { ago, shortSha } from "@/lib/format"
import { problemMessage } from "@/lib/problems"
import { platformLabel, protectionGuide } from "@/lib/repositories"

export const Route = createFileRoute("/admin/repositories/$name/")({
  component: Health,
})

function Health() {
  const { name } = Route.useParams()
  const connection = useQuery(getRepositoryOptions({ path: { repository: name } }))
  const health = useQuery({
    ...repositoryHealthOptions({ path: { repository: name } }),
    refetchInterval: 30_000,
  })
  const access = useMutation(testRepositoryAccessMutation())
  const doc = connection.data?.document
  const h = health.data
  const error = connection.error ?? health.error
  return (
    <section aria-labelledby="health-title" className="grid gap-6">
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div className="grid gap-1">
          <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
            <Link to="/admin/repositories" className="hover:text-foreground">
              Repositories
            </Link>{" "}
            / {name}
          </nav>
          <h1 id="health-title" className="text-2xl font-semibold tracking-tight">
            {doc?.metadata.title || name}
          </h1>
          {doc && (
            <p className="text-muted-foreground text-sm break-all">
              {platformLabel(doc.spec.connector)} · {doc.spec.url} · {connection.data?.managedBy === "config" ? "declared in configuration" : "managed in Quai"}
            </p>
          )}
        </div>
        <div className="flex flex-wrap gap-2">
          {doc && (
            <Button variant="outline" disabled={access.isPending} onClick={() => access.mutate({ body: doc })}>
              {access.isPending ? <Loader2 className="animate-spin" /> : <ScanSearch />} Test access
            </Button>
          )}
          <Link to="/admin/repositories/$name/edit" params={{ name }} className={buttonVariants({ variant: "outline" })}>
            <Pencil /> Edit
          </Link>
        </div>
      </header>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {health.isPending && <Skeleton className="h-64" />}
      {h && !h.connected && (
        <Alert variant="destructive" role="alert">
          <AlertTitle>Not connected</AlertTitle>
          <AlertDescription>{h.error || "Quai is connecting it."}</AlertDescription>
        </Alert>
      )}
      {access.isError && <Problems error={access.error} />}
      {access.data && (
        <Card>
          <CardHeader>
            <CardTitle>Access test</CardTitle>
          </CardHeader>
          <CardContent>
            <AccessReport report={access.data} />
          </CardContent>
        </Card>
      )}
      {h && h.connected && doc && (
        <Sections h={h} branch={doc.spec.defaultBranch || "main"} connector={doc.spec.connector} check={doc.spec.enforcement?.requiredCheck || "quai/approval"} />
      )}
      {h?.connected && doc?.spec.webhooks?.secret?.ref && (
        <Card>
          <CardHeader>
            <CardTitle>Webhook</CardTitle>
          </CardHeader>
          <CardContent>
            <InstallWebhook repository={name} />
          </CardContent>
        </Card>
      )}
      {h?.connected && <StackRuns repository={name} />}
    </section>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </>
  )
}

const when = (t?: string) => (t ? ago(t) : "never")

function Sections({ h, branch, connector, check }: { h: HealthView; branch: string; connector: string; check: string }) {
  const problems = h.definitions.problems ?? []
  const enforced = !h.enforcement.required || (h.enforcement.problems?.length ?? 0) === 0
  return (
    <div className="grid gap-6 lg:grid-cols-2">
      <Card className="lg:col-span-2">
        <CardHeader>
          <CardTitle>Enforcement</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-3">
          <EnforcementResult enforcement={h.enforcement} branch={branch} check={check} />
          {!enforced && (
            <ol className="list-decimal space-y-1 pl-5 text-sm" aria-label="Branch protection to set">
              {protectionGuide(connector, branch, check).map((s) => (
                <li key={s}>{s}</li>
              ))}
            </ol>
          )}
          {h.enforcement.checkedAt && <p className="text-muted-foreground text-xs">Verified {ago(h.enforcement.checkedAt)}; again every hour and when a branch policy changes.</p>}
          <TrustWarning problems={h.trust.problems} />
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Credential</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
            <Row label="Reference">{h.credentials.ref ? <code>{h.credentials.ref}</code> : "none"}</Row>
            <Row label="Readable">{h.credentials.ref ? h.credentials.readable ? "yes" : <span className="text-destructive">no: {h.credentials.error}</span> : "—"}</Row>
            <Row label="Expires">
              {h.credentials.expiresAt ? (
                <span className={h.credentials.expiresSoon ? "text-destructive" : undefined}>
                  {new Date(h.credentials.expiresAt).toLocaleDateString()}
                  {h.credentials.expiresSoon && " (within 30 days)"}
                </span>
              ) : (
                "not known"
              )}
            </Row>
          </dl>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Sync</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
            <Row label="Last webhook">{when(h.sync.lastWebhookAt)}</Row>
            <Row label="Last fetch">{when(h.sync.lastFetchAt)}</Row>
            <Row label="Last poll">{when(h.sync.lastPollAt)}</Row>
            <Row label="Head">{h.sync.head ? <code>{shortSha(h.sync.head)}</code> : "—"}</Row>
          </dl>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Definitions</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-3 text-sm">
          <p>
            {h.definitions.types} types and {h.definitions.blueprints} blueprints
            {h.definitions.commit && (
              <>
                {" "}
                at <code>{shortSha(h.definitions.commit)}</code>
              </>
            )}
            .
          </p>
          {problems.length > 0 && (
            <ul aria-label="Definition problems" className="grid gap-1">
              {problems.map((p) => (
                <li key={`${p.path}:${p.field}:${p.message}`}>
                  <code>{p.path}</code>
                  {p.field && <span className="text-muted-foreground"> {p.field}</span>}: <span className="text-destructive">{p.message}</span>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Pull requests</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
            <Row label="Open">{h.pullRequests.open}</Row>
            <Row label="Oldest">{h.pullRequests.oldestOpenedAt ? `opened ${ago(h.pullRequests.oldestOpenedAt)}` : "—"}</Row>
            <Row label="Stale">{h.pullRequests.stale > 0 ? <Badge variant="outline">{h.pullRequests.stale}</Badge> : "0"}</Row>
          </dl>
        </CardContent>
      </Card>
    </div>
  )
}
