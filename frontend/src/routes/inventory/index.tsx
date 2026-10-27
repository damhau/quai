import { useMemo, useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { Search } from "lucide-react"
import { getMeOptions, listInstancesOptions, listInventoryOptions } from "@/api/generated/@tanstack/react-query.gen"
import type { ResourceView } from "@/api/generated/types.gen"
import { ResourceStatusBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ago } from "@/lib/format"
import { problemMessage } from "@/lib/problems"
import { resourceParams } from "@/lib/resources"

export const Route = createFileRoute("/inventory/")({
  component: Inventory,
})

const text = (r: ResourceView) => `${r.key} ${r.path} ${r.owner ?? ""} ${Object.values(r.params ?? {}).join(" ")}`.toLowerCase()

function Inventory() {
  const { data, isPending, error } = useQuery({ ...listInventoryOptions(), refetchInterval: 30_000 })
  const { data: me } = useQuery(getMeOptions())
  const { data: instances } = useQuery({ ...listInstancesOptions(), refetchInterval: 30_000 })
  const [q, setQ] = useState("")
  const [type, setType] = useState("")
  const [mine, setMine] = useState(false)
  const resources = useMemo(() => data ?? [], [data])
  const types = useMemo(() => [...new Set(resources.map((r) => `${r.repository}/${r.type}`))].sort(), [resources])
  const shown = resources.filter((r) => (!type || `${r.repository}/${r.type}` === type) && (!mine || r.owner === me?.name) && q.toLowerCase().split(/\s+/).every((w) => text(r).includes(w)))
  return (
    <section aria-labelledby="inventory-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="inventory-title" className="text-2xl font-semibold tracking-tight">
          Inventory
        </h1>
        <p className="text-muted-foreground">Read from Git: every file that matches a resource type, including those that existed before Quai.</p>
      </header>
      <div className="flex flex-wrap items-center gap-4">
        <div className="relative w-full max-w-xs">
          <Search className="text-muted-foreground absolute top-1/2 left-2.5 size-4 -translate-y-1/2" aria-hidden />
          <Input className="pl-8" placeholder="Search name, path, owner" aria-label="Search the inventory" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <NativeSelect aria-label="Type" value={type} onChange={(e) => setType(e.target.value)}>
          <NativeSelectOption value="">All types</NativeSelectOption>
          {types.map((t) => (
            <NativeSelectOption key={t} value={t}>
              {t}
            </NativeSelectOption>
          ))}
        </NativeSelect>
        <div className="flex items-center gap-2">
          <Switch id="mine" checked={mine} onCheckedChange={setMine} />
          <Label htmlFor="mine">Owned by me</Label>
        </div>
      </div>
      {(instances ?? []).length > 0 && (
        <section aria-labelledby="deployments-title" className="grid gap-2">
          <h2 id="deployments-title" className="text-lg font-medium">
            Deployments
          </h2>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Deployment</TableHead>
                <TableHead>Blueprint</TableHead>
                <TableHead>Owner</TableHead>
                <TableHead>Resources</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(instances ?? [])
                .filter((i) => (!mine || i.owner === me?.name) && q.toLowerCase().split(/\s+/).every((w) => `${i.name} ${i.blueprint} ${i.owner}`.toLowerCase().includes(w)))
                .map((i) => (
                  <TableRow key={`${i.repository}/${i.blueprint}/${i.name}`}>
                    <TableCell>
                      <Link to="/deployments/$repository/$blueprint/$name" params={{ repository: i.repository, blueprint: i.blueprint, name: i.name }} className="font-medium hover:underline">
                        {i.name}
                      </Link>
                    </TableCell>
                    <TableCell className="text-sm">
                      {i.blueprint} <span className="text-muted-foreground">· {i.repository}</span>
                    </TableCell>
                    <TableCell className="text-sm">{i.owner}</TableCell>
                    <TableCell className="text-sm">{(i.resources ?? []).length}</TableCell>
                  </TableRow>
                ))}
            </TableBody>
          </Table>
        </section>
      )}
      <h2 className="text-lg font-medium">Resources</h2>
      {isPending && <Skeleton className="h-40" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {data && !shown.length && <p className="text-muted-foreground">{resources.length ? "Nothing matches." : "No resources in the connected repositories yet."}</p>}
      {shown.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Resource</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Owner</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Last apply</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((r) => (
              <TableRow key={r.id}>
                <TableCell>
                  <Link to="/resources/$repository/$type/$" params={resourceParams(r.id)} className="font-medium hover:underline">
                    {r.key}
                  </Link>
                  <p className="text-muted-foreground font-mono text-xs">{r.path}</p>
                </TableCell>
                <TableCell className="text-sm">
                  {r.type} <span className="text-muted-foreground">· {r.repository}</span>
                </TableCell>
                <TableCell className="text-sm">{r.owner ?? <span className="text-muted-foreground">none</span>}</TableCell>
                <TableCell>
                  <ResourceStatusBadge status={r.status} />
                </TableCell>
                <TableCell className="text-muted-foreground text-xs">{r.lastApply ? `${r.lastApply.status} ${ago(r.lastApply.startedAt)}` : "not reported"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}
