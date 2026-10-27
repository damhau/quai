import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { listRequestsOptions } from "@/api/generated/@tanstack/react-query.gen"
import { RequestTable } from "@/components/requests/request-table"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { problemMessage } from "@/lib/problems"
import { cn } from "@/lib/utils"

type View = "mine" | "all"

export const Route = createFileRoute("/requests/")({
  validateSearch: (search: Record<string, unknown>): { view: View } => ({ view: search.view === "all" ? "all" : "mine" }),
  component: Requests,
})

function Requests() {
  const { view } = Route.useSearch()
  const { data, isPending, error } = useQuery({ ...listRequestsOptions({ query: { view } }), refetchInterval: 10_000 })
  const requests = data ?? []
  return (
    <section aria-labelledby="requests-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="requests-title" className="text-2xl font-semibold tracking-tight">
          Requests
        </h1>
        <p className="text-muted-foreground">Everything asked of Quai, from submission to the commit.</p>
      </header>
      <nav aria-label="Requests" className="bg-muted text-muted-foreground inline-flex w-fit rounded-lg p-1 text-sm">
        {(["mine", "all"] as const).map((v) => (
          <Link
            key={v}
            to="/requests"
            search={{ view: v }}
            aria-current={view === v ? "page" : undefined}
            className={cn("rounded-md px-3 py-1", view === v && "bg-background text-foreground shadow-sm")}
          >
            {v === "mine" ? "Mine" : "All I can see"}
          </Link>
        ))}
      </nav>
      {isPending && <Skeleton className="h-40" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {data && !requests.length && (
        <p className="text-muted-foreground">
          No requests yet.{" "}
          <Link to="/" className="underline">
            Browse the catalog
          </Link>
        </p>
      )}
      {requests.length > 0 && <RequestTable requests={requests} showRequester={view === "all"} />}
    </section>
  )
}
