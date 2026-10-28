import { Eye, GitBranch } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"

/** PreviewOf is what a preview shows: a branch or a pull request, at a commit. */
export type PreviewOf = { label: string; commit: string }

/** PreviewBanner says the form comes from unmerged definitions. */
export function PreviewBanner({ preview }: { preview: PreviewOf }) {
  return (
    <Alert>
      <Eye />
      <AlertTitle>Preview of {preview.label}</AlertTitle>
      <AlertDescription>
        <p>
          The definitions at <code className="font-mono">{preview.commit.slice(0, 12)}</code>, before they are merged. Try the form: nothing can be requested from a preview.
        </p>
      </AlertDescription>
    </Alert>
  )
}

/** PreviewSubmit stands where the submit button is: previews cannot be submitted. */
export function PreviewSubmit() {
  return (
    <div className="grid gap-2">
      <Button type="button" disabled className="w-fit">
        <GitBranch /> Merge the definitions to request this
      </Button>
      <p className="text-muted-foreground text-xs">Previews cannot be submitted.</p>
    </div>
  )
}

/** PreviewPlan stands where the plan is: plans use the merged definitions only. */
export function PreviewPlan() {
  return (
    <Card role="region" aria-label="What will be committed">
      <CardHeader>
        <CardTitle>No plan in a preview</CardTitle>
        <CardDescription>Quai plans changes with the definitions of the default branch, once these are merged.</CardDescription>
      </CardHeader>
      <CardContent />
    </Card>
  )
}
