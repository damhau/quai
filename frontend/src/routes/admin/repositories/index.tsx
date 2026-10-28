import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { Plus } from "lucide-react"
import { listRepositoriesOptions, listRepositoryConnectionsOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { problemMessage } from "@/lib/problems"
import { connectionStatus, platformLabel } from "@/lib/repositories"
import { cn } from "@/lib/utils"

export const Route = createFileRoute("/admin/repositories/")({
  component: Repositories,
})

const tones = {
  ok: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
  warning: "bg-amber-500/10 text-amber-700 dark:text-amber-300",
  danger: "bg-destructive/10 text-destructive",
}

function Repositories() {
  const {
    data: connections,
    isPending,
    error,
  } = useQuery({
    ...listRepositoryConnectionsOptions(),
    refetchInterval: 30_000,
  })
  const { data: repos } = useQuery(listRepositoriesOptions())
  return (
    <section aria-labelledby="repositories-title" className="grid gap-6">
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div className="grid gap-1">
          <h1 id="repositories-title" className="text-2xl font-semibold tracking-tight">
            Repositories
          </h1>
          <p className="text-muted-foreground">The repositories Quai works with, and their health.</p>
        </div>
        <Link to="/admin/repositories/new" className={buttonVariants()}>
          <Plus /> Connect a repository
        </Link>
      </header>
      {isPending && <Skeleton className="h-40" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {connections && (
        <Table aria-label="Repositories">
          <TableHeader>
            <TableRow>
              <TableHead>Repository</TableHead>
              <TableHead>Platform</TableHead>
              <TableHead>Delivery</TableHead>
              <TableHead>Managed in</TableHead>
              <TableHead>Health</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {connections.map((c) => {
              const status = connectionStatus(
                c,
                repos?.find((r) => r.name === c.name),
              )
              return (
                <TableRow key={c.name}>
                  <TableCell>
                    <Link to="/admin/repositories/$name" params={{ name: c.name }} className="font-medium hover:underline">
                      {c.document.metadata.title || c.name}
                    </Link>
                    <div className="text-muted-foreground text-xs">{c.name}</div>
                  </TableCell>
                  <TableCell>{platformLabel(c.document.spec.connector)}</TableCell>
                  <TableCell>{c.document.spec.delivery?.mode === "direct" ? "Direct" : "Pull requests"}</TableCell>
                  <TableCell>{c.managedBy === "config" ? "Configuration" : "Quai"}</TableCell>
                  <TableCell>
                    <Badge variant="outline" className={cn("border-0", tones[status.tone])}>
                      {status.label}
                    </Badge>
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
      {connections?.length === 0 && <p className="text-muted-foreground">No repository yet: connect the first one.</p>}
    </section>
  )
}
