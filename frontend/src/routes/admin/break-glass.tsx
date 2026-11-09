import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { ShieldAlert } from "lucide-react"
import { listBreakGlassOptions } from "@/api/generated/@tanstack/react-query.gen"
import { StateBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { ago } from "@/lib/format"
import { problemMessage } from "@/lib/problems"

type Search = { open?: boolean }

export const Route = createFileRoute("/admin/break-glass")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    open: s.open === false || s.open === "false" ? false : undefined,
  }),
  component: BreakGlassReviews,
})

/**
 * BreakGlassReviews lists the requests merged by break-glass for auditors and platform admins,
 * the open post-incident reviews first (docs/design/05-delivery-and-approvals.md#break-glass).
 */
function BreakGlassReviews() {
  const { open: openOnly = true } = Route.useSearch()
  const navigate = Route.useNavigate()
  const list = useQuery(listBreakGlassOptions({ query: { open: openOnly } }))
  return (
    <section aria-labelledby="break-glass-title" className="grid gap-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h1 id="break-glass-title" className="text-2xl font-semibold tracking-tight">
            Break-glass reviews
          </h1>
          <p className="text-muted-foreground">
            Requests merged without their approvals in an incident. Each review stays open until two reviewers sign it off.
          </p>
        </div>
        <ToggleGroup
          type="single"
          variant="outline"
          value={openOnly ? "open" : "all"}
          onValueChange={(v) =>
            v &&
            void navigate({
              search: v === "open" ? {} : { open: false },
              replace: true,
            })
          }
          aria-label="Which reviews"
        >
          <ToggleGroupItem value="open">Open</ToggleGroupItem>
          <ToggleGroupItem value="all">All</ToggleGroupItem>
        </ToggleGroup>
      </header>
      {list.isError ? (
        <Alert variant="destructive">
          <ShieldAlert />
          <AlertTitle>The reviews cannot be listed</AlertTitle>
          <AlertDescription>{problemMessage(list.error)}</AlertDescription>
        </Alert>
      ) : list.isPending ? (
        <Skeleton className="h-32" />
      ) : (list.data ?? []).length === 0 ? (
        <p className="text-muted-foreground">{openOnly ? "No break-glass review is open." : "Glass was never broken."}</p>
      ) : (
        <Table aria-label="Break-glass reviews">
          <TableHeader>
            <TableRow>
              <TableHead>Request</TableHead>
              <TableHead>Incident</TableHead>
              <TableHead>Broken by</TableHead>
              <TableHead>Review</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(list.data ?? []).map((item) => (
              <TableRow key={item.request}>
                <TableCell>
                  <Link to="/requests/$id" params={{ id: item.request }} search={{}} className="font-medium underline-offset-4 hover:underline">
                    #{item.number}
                  </Link>{" "}
                  <span className="text-muted-foreground font-mono text-xs">{item.target}</span> <StateBadge state={item.state} />
                </TableCell>
                <TableCell>
                  <code className="font-mono">{item.breakGlass.incident}</code>
                </TableCell>
                <TableCell>
                  {item.breakGlass.by} <span className="text-muted-foreground text-xs">{ago(item.breakGlass.at)}</span>
                </TableCell>
                <TableCell>
                  {item.breakGlass.open ? (
                    <Badge variant="destructive">
                      Open · {(item.breakGlass.signOffs ?? []).length} of {item.breakGlass.needed}
                    </Badge>
                  ) : (
                    <Badge variant="secondary">Closed</Badge>
                  )}
                  {item.breakGlass.canSignOff && <span className="text-muted-foreground ml-2 text-xs">yours to sign off</span>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}
