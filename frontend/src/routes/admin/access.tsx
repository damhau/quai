import { useState } from "react"
import { createFileRoute } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { Download } from "lucide-react"
import { getAccessReportOptions } from "@/api/generated/@tanstack/react-query.gen"
import type { AccessRoleView, AccessScimTokenView, AccessUserView } from "@/api/generated/types.gen"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { accessExportURL, matches } from "@/lib/access"
import { problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/admin/access")({ component: AccessReport })

const when = (at: string) => (
  <time dateTime={at} title={at}>
    {new Date(at).toLocaleString()}
  </time>
)

function AccessReport() {
  const report = useQuery(getAccessReportOptions())
  const [q, setQ] = useState("")
  const users = report.data?.users ?? []
  const shown = users.filter((u) => matches(u, q))
  const active = users.filter((u) => u.state === "active").length
  return (
    <section aria-labelledby="access-title" className="grid gap-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h1 id="access-title" className="text-2xl font-semibold tracking-tight">
            Access report
          </h1>
          <p className="text-muted-foreground">
            Everyone who has signed in or holds an API token: the groups of their last sign-in, and the teams and roles the
            current configuration gives them. A token acts with the groups its owner had when creating it. For someone the
            identity provider provisions (SCIM), only the groups it still gives them count, in sessions and tokens alike.
          </p>
        </div>
        <a href={accessExportURL} download className={buttonVariants({ variant: "outline" })}>
          <Download />
          Download CSV
        </a>
      </header>
      {report.isPending && <Skeleton className="h-64" />}
      {report.error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(report.error)}</AlertDescription>
        </Alert>
      )}
      {report.data && (
        <>
          <div className="flex flex-wrap items-end justify-between gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="access-filter">Filter</Label>
              <Input
                id="access-filter"
                type="search"
                className="w-72"
                value={q}
                onChange={(e) => setQ(e.target.value)}
                placeholder="name, email, group, team, role or token"
              />
            </div>
            <p className="text-muted-foreground text-sm">
              {users.length} {users.length === 1 ? "person" : "people"}, {active} active. Generated {when(report.data.generatedAt)}.
            </p>
          </div>
          {users.length === 0 && <p className="text-muted-foreground">No one has signed in yet.</p>}
          {users.length > 0 && shown.length === 0 && <p className="text-muted-foreground">No one matches.</p>}
          {shown.length > 0 && (
            <Table aria-label="Users">
              <TableHeader>
                <TableRow>
                  <TableHead>Person</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Groups</TableHead>
                  <TableHead>Teams</TableHead>
                  <TableHead>Roles</TableHead>
                  <TableHead>API tokens</TableHead>
                  <TableHead>Last sign-in</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((u) => (
                  <Person key={u.name} user={u} />
                ))}
              </TableBody>
            </Table>
          )}
          <ScimTokens tokens={report.data.scimTokens ?? []} />
        </>
      )}
    </section>
  )
}

function Person({ user: u }: { user: AccessUserView }) {
  const roles = u.roles ?? []
  const tokens = u.tokens ?? []
  return (
    <TableRow className="align-top">
      <TableCell className="min-w-48 whitespace-normal">
        <div className="font-medium break-all">{u.name}</div>
        {u.email && <div className="text-muted-foreground text-xs break-all">{u.email}</div>}
        {u.subject && <div className="text-muted-foreground font-mono text-xs break-all">sub {u.subject}</div>}
        {u.objectId && <div className="text-muted-foreground font-mono text-xs break-all">oid {u.objectId}</div>}
      </TableCell>
      <TableCell>
        {u.state === "deactivated" ? (
          <Badge variant="destructive" title={u.deactivatedAt}>
            Deactivated
          </Badge>
        ) : (
          <Badge variant="secondary">Active</Badge>
        )}
        {u.provisioned && (
          <Badge variant="outline" className="mt-1 block w-fit" title="The identity provider's provisioning (SCIM) keeps this person current">
            Provisioned
          </Badge>
        )}
      </TableCell>
      <TableCell className="min-w-40 text-sm whitespace-normal">{list(u.groups)}</TableCell>
      <TableCell className="min-w-32 text-sm whitespace-normal">{list(u.teams)}</TableCell>
      <TableCell className="min-w-56 whitespace-normal">
        <Roles roles={roles} />
      </TableCell>
      <TableCell className="min-w-56 whitespace-normal">
        {tokens.length === 0 ? (
          <span className="text-muted-foreground text-sm">None</span>
        ) : (
          <ul className="grid gap-2" aria-label={`API tokens of ${u.name}`}>
            {tokens.map((t) => (
              <li key={t.id} className="text-sm">
                <span className="font-medium break-all">{t.name}</span>{" "}
                <span className="text-muted-foreground text-xs">{(t.scopes ?? []).join(", ")}</span>
                <div className="text-muted-foreground text-xs">expires {when(t.expiresAt)}</div>
                <Roles roles={t.roles ?? []} />
              </li>
            ))}
          </ul>
        )}
      </TableCell>
      <TableCell className="text-xs whitespace-nowrap">
        {u.lastLogin ? when(u.lastLogin) : <span className="text-muted-foreground">No sign-in recorded</span>}
        {u.authTime && <div className="text-muted-foreground">authenticated {when(u.authTime)}</div>}
      </TableCell>
    </TableRow>
  )
}

/** ScimTokens lists the SCIM tokens the identity provider's provisioning used, and when last. */
function ScimTokens({ tokens }: { tokens: AccessScimTokenView[] }) {
  if (tokens.length === 0) return null
  return (
    <section aria-labelledby="scim-title" className="grid gap-2">
      <h2 id="scim-title" className="text-lg font-semibold">
        Provisioning (SCIM)
      </h2>
      <p className="text-muted-foreground text-sm">
        The tokens of <code>auth.scim.tokens</code> the identity provider used: they can deactivate people and change their
        groups.
      </p>
      <ul className="grid gap-1 text-sm" aria-label="SCIM tokens">
        {tokens.map((t) => (
          <li key={t.name}>
            <span className="font-medium">{t.name}</span> <span className="text-muted-foreground">last used {when(t.lastUsedAt)}</span>
          </li>
        ))}
      </ul>
    </section>
  )
}

/** Roles lists roles at their scopes, each with the groups that give it. */
function Roles({ roles }: { roles: AccessRoleView[] }) {
  if (roles.length === 0) return <span className="text-muted-foreground text-sm">No role</span>
  return (
    <ul className="grid gap-1">
      {roles.map((r) => (
        <li key={`${r.role}@${r.scope}`} className="text-sm">
          <span className="font-medium">{r.role}</span> <span className="text-muted-foreground">·</span> {r.scope}
          <div className="text-muted-foreground text-xs break-all">from {(r.groups ?? []).join(", ")}</div>
        </li>
      ))}
    </ul>
  )
}

const list = (items: string[] | null | undefined) =>
  items?.length ? <span className="break-all">{items.join(", ")}</span> : <span className="text-muted-foreground">None</span>
