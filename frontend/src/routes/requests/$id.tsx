import { createFileRoute } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { getRequestOptions } from "@/api/generated/@tanstack/react-query.gen"
import { RulesPreview } from "@/components/approval/rules-preview"
import { ChangesView } from "@/components/diff/changes-view"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"

// The request's summary; the timeline and the review come with P0-23.

export const Route = createFileRoute("/requests/$id")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getRequestOptions({ path: params })),
  component: RequestPage,
})

function RequestPage() {
  const { id } = Route.useParams()
  const { data: r } = useSuspenseQuery(getRequestOptions({ path: { id } }))
  return (
    <div className="grid gap-6">
      <header className="grid gap-1">
        <div className="flex items-center gap-2">
          <h1 className="text-2xl font-semibold tracking-tight">Request #{r.number}</h1>
          <Badge variant="secondary">{r.state}</Badge>
        </div>
        <p className="text-muted-foreground text-sm">
          {r.kind} <code className="font-mono">{r.target}</code> · by {r.requester}
        </p>
        {r.reason && <p>{r.reason}</p>}
      </header>
      <div className="grid items-start gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Approvals</CardTitle>
          </CardHeader>
          <CardContent>
            <RulesPreview rules={r.rules ?? []} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Changes</CardTitle>
          </CardHeader>
          <CardContent>
            <ChangesView changes={r.changes ?? []} />
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
