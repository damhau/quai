import { useState } from "react"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery } from "@tanstack/react-query"
import { Download, ShieldAlert } from "lucide-react"
import { getMeOptions, listEvidenceRequestsOptions, listRepositoriesOptions } from "@/api/generated/@tanstack/react-query.gen"
import type { EvidenceRequestView } from "@/api/generated/types.gen"
import { StateBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { dayEnd, dayStart } from "@/lib/audit"
import { evidenceURL, maxArchive, sampledRepositories, saveArchive } from "@/lib/evidence"
import { problemMessage } from "@/lib/problems"
import { states } from "@/lib/requests"

const sampleStates = ["pending_approval", "approved", "merging", "applying", "applied", "failed", "rejected", "cancelled", "stale", "expired"] as const
type SampleState = (typeof sampleStates)[number]

type Search = { from?: string; to?: string; repository?: string; state?: SampleState; breakGlass?: boolean }

const text = (v: unknown) => (typeof v === "string" && v.trim() !== "" ? v.trim() : undefined)
const day = (v: unknown) => (typeof v === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v) ? v : undefined)
const state = (v: unknown) => sampleStates.find((s) => s === v)

export const Route = createFileRoute("/admin/evidence")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    from: day(s.from),
    to: day(s.to),
    repository: text(s.repository),
    state: state(s.state),
    breakGlass: s.breakGlass === true || s.breakGlass === "true" ? true : undefined,
  }),
  component: Evidence,
})

/** The most requests a sample lists; the server says when more match. */
const listed = 500

/** localDay writes a date as a local day, yyyy-mm-dd. */
function localDay(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** The period shown without one in the address: the last 30 days. */
function lastDays(n: number) {
  const from = new Date()
  from.setDate(from.getDate() - n)
  return { from: localDay(from), to: localDay(new Date()) }
}

/**
 * Evidence lets auditors sample requests by period, repository, state and break-glass, and download
 * their evidence bundles, each one or a selection as one archive
 * (docs/design/09-security-and-compliance.md#evidence-bundle).
 */
function Evidence() {
  const search = Route.useSearch()
  const me = useQuery(getMeOptions())
  const scoped = sampledRepositories(me.data)
  const [defaults] = useState(() => lastDays(30))
  // An auditor of some repositories samples one of them at a time.
  const period = { from: search.from ?? defaults.from, to: search.to ?? defaults.to, repository: search.repository ?? scoped?.[0] }
  const since = dayStart(period.from)
  const until = dayEnd(period.to)
  const list = useQuery({
    ...listEvidenceRequestsOptions({
      query: { since: since ?? "", until: until ?? "", repository: period.repository, state: search.state, breakGlass: search.breakGlass, limit: listed },
    }),
    enabled: me.isSuccess && since !== undefined && until !== undefined,
  })
  return (
    <section aria-labelledby="evidence-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="evidence-title" className="text-2xl font-semibold tracking-tight">
          Evidence
        </h1>
        <p className="text-muted-foreground">
          The requests of a period, to sample: each one's evidence bundle (JSON, to verify offline with <code className="font-mono">quai audit verify --bundle</code>, and a PDF summary), or up to{" "}
          {maxArchive} of them as one archive.
        </p>
      </header>
      <Filters key={JSON.stringify({ ...search, ...period })} search={{ ...search, ...period }} scoped={scoped} />
      {list.isError ? (
        <Alert variant="destructive">
          <ShieldAlert />
          <AlertTitle>The requests cannot be listed</AlertTitle>
          <AlertDescription>{problemMessage(list.error)}</AlertDescription>
        </Alert>
      ) : list.isPending ? (
        <Skeleton className="h-32" />
      ) : (
        <Sample key={JSON.stringify(search)} requests={list.data.requests ?? []} truncated={list.data.truncated} />
      )}
    </section>
  )
}

function Filters({ search, scoped }: { search: Search; scoped: string[] | undefined }) {
  const navigate = useNavigate({ from: "/admin/evidence" })
  const repositories = useQuery(listRepositoriesOptions())
  const shown = (repositories.data ?? []).filter((r) => scoped === undefined || scoped.includes(r.name))
  const [f, setF] = useState<Search>(search)
  const set = (k: "from" | "to" | "repository") => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setF({ ...f, [k]: e.target.value || undefined })
  return (
    <form
      aria-label="Sample"
      className="flex flex-wrap items-end gap-3"
      onSubmit={(e) => {
        e.preventDefault()
        void navigate({ search: f })
      }}
    >
      <div className="grid gap-1.5">
        <Label htmlFor="evidence-from">From</Label>
        <Input id="evidence-from" type="date" className="w-40" value={f.from ?? ""} onChange={set("from")} required />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="evidence-to">To</Label>
        <Input id="evidence-to" type="date" className="w-40" value={f.to ?? ""} onChange={set("to")} required />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="evidence-repository">Repository</Label>
        <NativeSelect id="evidence-repository" value={f.repository ?? ""} onChange={set("repository")}>
          {scoped === undefined && <NativeSelectOption value="">All</NativeSelectOption>}
          {shown.map((r) => (
            <NativeSelectOption key={r.name} value={r.name}>
              {r.title || r.name}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="evidence-state">State</Label>
        <NativeSelect id="evidence-state" value={f.state ?? ""} onChange={(e) => setF({ ...f, state: state(e.target.value) })}>
          <NativeSelectOption value="">All</NativeSelectOption>
          {sampleStates.map((s) => (
            <NativeSelectOption key={s} value={s}>
              {states[s]?.label ?? s}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>
      <div className="flex h-9 items-center gap-2">
        <Checkbox id="evidence-break-glass" checked={f.breakGlass === true} onCheckedChange={(c) => setF({ ...f, breakGlass: c === true || undefined })} />
        <Label htmlFor="evidence-break-glass">Merged by break-glass</Label>
      </div>
      <Button type="submit">List</Button>
    </form>
  )
}

/** Sample lists the requests of a sample, a selection of them to download as one archive. */
function Sample({ requests, truncated }: { requests: EvidenceRequestView[]; truncated: boolean }) {
  const [selected, setSelected] = useState<string[]>([])
  const archive = useMutation({ mutationFn: () => saveArchive(selected) })
  if (requests.length === 0) return <p className="text-muted-foreground">No request in this period matches.</p>
  const toggle = (id: string, on: boolean) => setSelected(on ? [...selected, id] : selected.filter((s) => s !== id))
  const first = requests.slice(0, maxArchive).map((r) => r.id)
  const all = selected.length > 0 && first.every((id) => selected.includes(id))
  const full = selected.length >= maxArchive
  return (
    <div className="grid gap-4">
      {truncated && (
        <Alert>
          <AlertDescription>More requests match than the {listed} listed: narrow the period.</AlertDescription>
        </Alert>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={() => archive.mutate()} disabled={selected.length === 0 || archive.isPending}>
          <Download />
          {archive.isPending ? "Making the archive…" : `Download ${selected.length} as an archive`}
        </Button>
        <span className="text-muted-foreground text-sm">
          {full ? `An archive holds at most ${maxArchive} requests.` : `Select up to ${maxArchive} requests.`}
        </span>
      </div>
      {archive.isError && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(archive.error)}</AlertDescription>
        </Alert>
      )}
      <Table aria-label="Requests">
        <TableHeader>
          <TableRow>
            <TableHead className="w-8">
              <Checkbox
                aria-label={`Select the first ${Math.min(maxArchive, requests.length)}`}
                checked={all}
                onCheckedChange={(c) => setSelected(c === true ? first : [])}
              />
            </TableHead>
            <TableHead>Request</TableHead>
            <TableHead>Repository</TableHead>
            <TableHead>State</TableHead>
            <TableHead>Requested by</TableHead>
            <TableHead>Created</TableHead>
            <TableHead>Bundle</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {requests.map((r) => {
            const on = selected.includes(r.id)
            return (
              <TableRow key={r.id}>
                <TableCell>
                  <Checkbox aria-label={`Select #${r.number}`} checked={on} disabled={!on && full} onCheckedChange={(c) => toggle(r.id, c === true)} />
                </TableCell>
                <TableCell>
                  <Link to="/requests/$id" params={{ id: r.id }} search={{}} className="font-medium underline-offset-4 hover:underline">
                    #{r.number}
                  </Link>{" "}
                  <span className="text-muted-foreground font-mono text-xs">{r.target}</span>
                </TableCell>
                <TableCell className="font-mono text-xs">{r.repository}</TableCell>
                <TableCell>
                  <StateBadge state={r.state} /> {r.breakGlass && <Badge variant="destructive">Break-glass</Badge>}
                </TableCell>
                <TableCell>{r.requester}</TableCell>
                <TableCell className="text-xs whitespace-nowrap">
                  <time dateTime={r.createdAt}>{new Date(r.createdAt).toLocaleString()}</time>
                </TableCell>
                <TableCell className="text-sm whitespace-nowrap">
                  <a href={evidenceURL(r.id, "json")} download className="underline-offset-4 hover:underline" aria-label={`JSON bundle of #${r.number}`}>
                    JSON
                  </a>{" "}
                  ·{" "}
                  <a href={evidenceURL(r.id, "pdf")} download className="underline-offset-4 hover:underline" aria-label={`PDF summary of #${r.number}`}>
                    PDF
                  </a>
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </div>
  )
}
