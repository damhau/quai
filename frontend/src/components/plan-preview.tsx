import type { UseQueryResult } from "@tanstack/react-query"
import { AlertCircle, GitCommitHorizontal, Loader2 } from "lucide-react"
import type { PlanView } from "@/api/generated/types.gen"
import { RulesPreview } from "@/components/approval/rules-preview"
import { ChangesView } from "@/components/diff/changes-view"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { hasFieldErrors, problemMessage } from "@/lib/problems"

/**
 * PlanPreview shows what a request would commit and who would approve it, from the server's
 * plan of the form as it stands: the server is the only one that renders and validates.
 */
export function PlanPreview({ ready, hint, plan }: { ready: boolean; hint: string; plan: UseQueryResult<PlanView, unknown> }) {
  return (
    <Card className="lg:sticky lg:top-6" aria-labelledby="preview-title" role="region">
      <CardHeader>
        <CardTitle id="preview-title" className="flex items-center gap-2">
          <GitCommitHorizontal className="size-4" aria-hidden /> What will be committed
          {ready && plan.isFetching && <Loader2 className="text-muted-foreground size-4 animate-spin" aria-label="Updating" />}
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-5">
        {!ready ? (
          <p className="text-muted-foreground text-sm">{hint}</p>
        ) : plan.isError ? (
          hasFieldErrors(plan.error) ? (
            <p className="text-muted-foreground text-sm">Fix the highlighted fields to see the preview.</p>
          ) : (
            <Alert variant="destructive">
              <AlertCircle />
              <AlertDescription>{problemMessage(plan.error)}</AlertDescription>
            </Alert>
          )
        ) : !plan.data ? (
          <Skeleton className="h-40" />
        ) : (
          <>
            {plan.data.stack && (
              <p className="text-muted-foreground text-xs">
                Applied by the stack <code className="font-mono">{plan.data.stack}</code>
              </p>
            )}
            <section className="grid gap-2" aria-label="Approvals">
              <h3 className="text-sm font-medium">Approvals</h3>
              <RulesPreview rules={plan.data.rules ?? []} />
            </section>
            <section className="grid gap-2" aria-label="Changes">
              <h3 className="text-sm font-medium">Changes</h3>
              <ChangesView changes={plan.data.changes ?? []} />
            </section>
          </>
        )}
      </CardContent>
    </Card>
  )
}
