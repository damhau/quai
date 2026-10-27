import { createFileRoute } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { listRequestsOptions } from "@/api/generated/@tanstack/react-query.gen"
import { RequestTable } from "@/components/requests/request-table"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/approvals")({
  component: Approvals,
})

function Approvals() {
  const { data, isPending, error } = useQuery({ ...listRequestsOptions({ query: { view: "inbox" } }), refetchInterval: 10_000 })
  const requests = data ?? []
  return (
    <section aria-labelledby="approvals-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="approvals-title" className="text-2xl font-semibold tracking-tight">
          Approvals
        </h1>
        <p className="text-muted-foreground">Requests waiting for you. You review the exact change that will be committed.</p>
      </header>
      {isPending && <Skeleton className="h-40" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {data && !requests.length && <p className="text-muted-foreground">Nothing needs your approval right now.</p>}
      {requests.length > 0 && <RequestTable requests={requests} showRequester />}
    </section>
  )
}
