import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ScanSearch } from "lucide-react"
import { checkDriftMutation, listRunsOptions, listRunsQueryKey } from "@/api/generated/@tanstack/react-query.gen"
import type { RunView } from "@/api/generated/types.gen"
import { StateBadge } from "@/components/requests/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ago, shortSha, webURL } from "@/lib/format"
import { problemMessage } from "@/lib/problems"

/** StackRuns lists a repository's latest runs, by stack (docs/design/06); with a drift-check
 *  pipeline, it queues the drift checks of its stacks now. */
export function StackRuns({ repository, drift }: { repository: string; drift: boolean }) {
  const queryClient = useQueryClient()
  const { data } = useQuery(listRunsOptions({ path: { name: repository }, query: { limit: 100 } }))
  const check = useMutation({
    ...checkDriftMutation(),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: listRunsQueryKey({ path: { name: repository } }) }),
  })
  const byStack = new Map<string, RunView[]>()
  for (const r of data ?? []) byStack.set(r.stack, [...(byStack.get(r.stack) ?? []), r])
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <CardTitle>Runs</CardTitle>
        {drift && (
          <Button size="sm" variant="outline" disabled={check.isPending} onClick={() => check.mutate({ path: { name: repository }, body: {} })}>
            <ScanSearch /> Check drift
          </Button>
        )}
      </CardHeader>
      <CardContent className="grid gap-4 text-sm">
        {check.error && (
          <Alert variant="destructive">
            <AlertDescription>{problemMessage(check.error)}</AlertDescription>
          </Alert>
        )}
        {check.isSuccess && <p className="text-muted-foreground">Drift checks queued: {(check.data ?? []).map((r) => r.stack).join(", ")}.</p>}
        {byStack.size === 0 && <p className="text-muted-foreground">No run reported yet: the pipelines report with quai report.</p>}
        {[...byStack].map(([stack, runs]) => (
          <section key={stack} aria-label={`Runs of ${stack}`} className="grid gap-1">
            <h3 className="font-mono text-xs font-medium">{stack}</h3>
            <ul className="grid gap-1">
              {runs.slice(0, 5).map((r) => (
                <li key={r.id} className="flex flex-wrap items-center gap-2">
                  <span className="w-20">{r.kind}</span>
                  <StateBadge state={r.status} />
                  <code className="text-muted-foreground text-xs">{shortSha(r.commit)}</code>
                  {r.pullRequest && <span className="text-muted-foreground text-xs">pull request #{r.pullRequest}</span>}
                  <span className="text-muted-foreground text-xs">{ago(r.finishedAt ?? r.startedAt)}</span>
                  {webURL(r.url) && (
                    <a href={webURL(r.url)} target="_blank" rel="noreferrer" className="text-muted-foreground text-xs underline">
                      run
                    </a>
                  )}
                </li>
              ))}
            </ul>
          </section>
        ))}
      </CardContent>
    </Card>
  )
}
