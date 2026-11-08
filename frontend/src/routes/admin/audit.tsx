import { useState } from "react"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { useInfiniteQuery } from "@tanstack/react-query"
import { Download, ShieldAlert } from "lucide-react"
import { listAuditEventsInfiniteOptions } from "@/api/generated/@tanstack/react-query.gen"
import type { AuditEventView, AuditPageView } from "@/api/generated/types.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import {
  actionGroups,
  actionLabel,
  actorLabel,
  alarming,
  dayEnd,
  dayStart,
  details,
  erased,
  exportURL,
  objectLabel,
  clear,
  type Personal,
} from "@/lib/audit"
import { problemMessage } from "@/lib/problems"
import { cn } from "@/lib/utils"

type Search = { action?: string; actor?: string; object?: string; from?: string; to?: string }

const text = (v: unknown) => (typeof v === "string" && v.trim() !== "" ? v.trim() : undefined)
const day = (v: unknown) => (typeof v === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v) ? v : undefined)

export const Route = createFileRoute("/admin/audit")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    action: text(s.action),
    actor: text(s.actor),
    object: text(s.object),
    from: day(s.from),
    to: day(s.to),
  }),
  component: AuditLog,
})

const pageSize = 100

function AuditLog() {
  const search = Route.useSearch()
  const query = {
    limit: pageSize,
    action: search.action,
    actor: search.actor,
    object: search.object,
    since: search.from && dayStart(search.from),
    until: search.to && dayEnd(search.to),
  }
  const log = useInfiniteQuery({
    ...listAuditEventsInfiniteOptions({ query }),
    initialPageParam: 0,
    getNextPageParam: (page) => page.next,
  })
  const pages = log.data?.pages ?? []
  const events = pages.flatMap((p) => p.events ?? [])
  const personal: Personal = Object.assign({}, ...pages.map((p) => p.personal))
  const unprotected = pages[0]?.protected === false
  return (
    <section aria-labelledby="audit-title" className="grid gap-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h1 id="audit-title" className="text-2xl font-semibold tracking-tight">
            Audit log
          </h1>
          <p className="text-muted-foreground">
            Every security-relevant event, newest first, each chained to the one before by its hash.
          </p>
        </div>
        <a href={exportURL} download className={buttonVariants({ variant: "outline" })}>
          <Download />
          Export
        </a>
      </header>
      {pages[0] && <Integrity page={pages[0]} />}
      {unprotected && (
        <Alert variant="destructive">
          <ShieldAlert />
          <AlertTitle>The audit log is not protected</AlertTitle>
          <AlertDescription>
            Quai connects to the database as a role that can change or delete audit events. Connect the server and the worker as
            an application role (database.url) and run the migrations as the schema's owner (database.migrateURL).
          </AlertDescription>
        </Alert>
      )}
      <Filters key={JSON.stringify(search)} search={search} />
      {log.isPending && <Skeleton className="h-64" />}
      {log.error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(log.error)}</AlertDescription>
        </Alert>
      )}
      {log.data && events.length === 0 && <p className="text-muted-foreground">No event matches.</p>}
      {events.length > 0 && (
        <Table aria-label="Audit events">
          <TableHeader>
            <TableRow>
              <TableHead className="w-16">#</TableHead>
              <TableHead>When</TableHead>
              <TableHead>Who</TableHead>
              <TableHead>What</TableHead>
              <TableHead>About</TableHead>
              <TableHead>Details</TableHead>
              <TableHead>Hash</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((e) => (
              <Event key={e.seq} event={e} personal={personal} />
            ))}
          </TableBody>
        </Table>
      )}
      {log.hasNextPage && (
        <div>
          <Button variant="outline" onClick={() => void log.fetchNextPage()} disabled={log.isFetchingNextPage}>
            {log.isFetchingNextPage ? "Loading…" : "Older events"}
          </Button>
        </div>
      )}
    </section>
  )
}

const when = (at: string) => <time dateTime={at}>{new Date(at).toLocaleString()}</time>

/** Integrity says whether the chain held at its last daily verification, and its last checkpoint. */
function Integrity({ page }: { page: AuditPageView }) {
  const { checkpoint: c, verification: v } = page
  if (v && !v.verified) {
    return (
      <Alert variant="destructive">
        <ShieldAlert />
        <AlertTitle>The audit log does not verify</AlertTitle>
        <AlertDescription>
          <p>
            It breaks at seq {v.broken}: {v.reason}. Found {when(v.at)}.
          </p>
          <p>Follow the runbook "When the chain does not verify" in the operations guide before anything else changes.</p>
        </AlertDescription>
      </Alert>
    )
  }
  return (
    <p className="text-muted-foreground text-sm">
      {v ? (
        <>
          Verified {when(v.at)}: {v.events} events held.{" "}
        </>
      ) : (
        "Not verified yet: the worker verifies the chain every day. "
      )}
      {c ? (
        <>
          Last checkpoint at seq {c.seq}, signed {when(c.at)}.
        </>
      ) : (
        "No signed checkpoint yet."
      )}
    </p>
  )
}

function Filters({ search }: { search: Search }) {
  const navigate = useNavigate({ from: "/admin/audit" })
  const [f, setF] = useState<Search>(search)
  const set = (k: keyof Search) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setF({ ...f, [k]: e.target.value || undefined })
  return (
    <form
      aria-label="Filters"
      className="flex flex-wrap items-end gap-3"
      onSubmit={(e) => {
        e.preventDefault()
        void navigate({ search: f })
      }}
    >
      <div className="grid gap-1.5">
        <Label htmlFor="audit-action">Action</Label>
        <NativeSelect id="audit-action" value={f.action ?? ""} onChange={set("action")}>
          <NativeSelectOption value="">All</NativeSelectOption>
          {actionGroups.map((g) => (
            <NativeSelectOption key={g.value} value={g.value}>
              {g.label}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="audit-actor">Who</Label>
        <Input id="audit-actor" className="w-40" value={f.actor ?? ""} onChange={set("actor")} placeholder="name or repository" />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="audit-object">About</Label>
        <Input id="audit-object" className="w-56" value={f.object ?? ""} onChange={set("object")} placeholder="request, resource, repository" />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="audit-from">From</Label>
        <Input id="audit-from" type="date" className="w-40" value={f.from ?? ""} onChange={set("from")} />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="audit-to">To</Label>
        <Input id="audit-to" type="date" className="w-40" value={f.to ?? ""} onChange={set("to")} />
      </div>
      <Button type="submit">Filter</Button>
      {Object.values(search).some(Boolean) && (
        <Button type="button" variant="ghost" onClick={() => void navigate({ search: {} })}>
          Clear
        </Button>
      )}
    </form>
  )
}

function Event({ event: e, personal }: { event: AuditEventView; personal: Personal }) {
  const who = actorLabel(e.actor, personal)
  const ip = e.actor.ip && clear(e.actor.ip, personal)
  return (
    <TableRow className="align-top">
      <TableCell className="text-muted-foreground font-mono text-xs">{e.seq}</TableCell>
      <TableCell className="text-xs whitespace-nowrap">
        <time dateTime={e.at} title={e.at}>
          {new Date(e.at).toLocaleString()}
        </time>
      </TableCell>
      <TableCell className="text-sm">
        <div className={cn(who.startsWith(erased) && "text-muted-foreground italic")}>{who}</div>
        {e.actor.authTime && (
          <div className="text-muted-foreground text-xs">
            signed in <time dateTime={e.actor.authTime}>{new Date(e.actor.authTime).toLocaleString()}</time>
          </div>
        )}
        {ip && <div className="text-muted-foreground text-xs">{ip}</div>}
      </TableCell>
      <TableCell>
        {alarming(e.action) ? (
          <Badge variant="destructive">{actionLabel(e.action)}</Badge>
        ) : (
          <span className="text-sm font-medium">{actionLabel(e.action)}</span>
        )}
        <div className="text-muted-foreground font-mono text-xs">{e.action}</div>
      </TableCell>
      <TableCell className="text-sm">
        {e.object.type === "request" ? (
          <Link to="/requests/$id" params={{ id: e.object.id }} className="hover:underline">
            {objectLabel(e.object, personal)}
          </Link>
        ) : (
          objectLabel(e.object, personal)
        )}
        <div className="text-muted-foreground text-xs">{e.object.type}</div>
      </TableCell>
      <TableCell className="w-full min-w-64 whitespace-normal">
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 text-xs">
          {details(e, personal).map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className={cn("break-all", v === erased && "text-muted-foreground italic")}>{v}</dd>
            </div>
          ))}
        </dl>
      </TableCell>
      <TableCell>
        <code className="font-mono text-xs" title={`hash ${e.hash}\nprev ${e.prev}`}>
          {e.hash.slice(0, 12)}
        </code>
      </TableCell>
    </TableRow>
  )
}
