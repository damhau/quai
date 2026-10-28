import { useState } from "react"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { Eye } from "lucide-react"
import { previewDefinitionsOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { previewLabel, previewSearch } from "@/lib/preview"
import { problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/preview/$repository/")({
  validateSearch: previewSearch,
  component: Preview,
})

/** Preview lets catalog authors try the definitions of a branch or a pull request before merging. */
function Preview() {
  const { repository } = Route.useParams()
  const search = Route.useSearch()
  const navigate = useNavigate()
  const [kind, setKind] = useState<"branch" | "pullRequest">(search.pullRequest ? "pullRequest" : "branch")
  const [ref, setRef] = useState(search.pullRequest ?? search.branch ?? "")
  const chosen = Boolean(search.branch || search.pullRequest)
  const { data, isPending, error } = useQuery({ ...previewDefinitionsOptions({ path: { repository }, query: search }), enabled: chosen })
  return (
    <section aria-labelledby="preview-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="preview-title" className="text-2xl font-semibold tracking-tight">
          Preview definitions of {repository}
        </h1>
        <p className="text-muted-foreground">Try types and blueprints from a branch or an open pull request before merging them. Nothing can be requested from a preview.</p>
      </header>
      <Card>
        <CardContent>
          <form
            className="flex flex-wrap items-end gap-3"
            aria-label="What to preview"
            onSubmit={(e) => {
              e.preventDefault()
              void navigate({ to: "/preview/$repository", params: { repository }, search: kind === "branch" ? { branch: ref } : { pullRequest: ref } })
            }}
          >
            <ToggleGroup type="single" variant="outline" size="sm" aria-label="Preview a" value={kind} onValueChange={(k) => k && setKind(k as "branch" | "pullRequest")}>
              <ToggleGroupItem value="branch" className="px-3">
                Branch
              </ToggleGroupItem>
              <ToggleGroupItem value="pullRequest" className="px-3">
                Pull request
              </ToggleGroupItem>
            </ToggleGroup>
            <div className="grid gap-2">
              <Label htmlFor="preview-ref">{kind === "branch" ? "Branch" : "Pull request ID"}</Label>
              <Input id="preview-ref" className="w-64" value={ref} onChange={(e) => setRef(e.target.value)} placeholder={kind === "branch" ? "feature/vm-v2" : "42"} />
            </div>
            <Button type="submit" disabled={ref.trim() === ""}>
              <Eye /> Preview
            </Button>
          </form>
        </CardContent>
      </Card>
      {chosen && isPending && <Skeleton className="h-32" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {data && (
        <>
          <p className="text-muted-foreground text-sm">
            {previewLabel(search)} at <code className="font-mono">{data.commit.slice(0, 12)}</code>
          </p>
          {(data.problems ?? []).length > 0 && (
            <Alert variant="destructive">
              <AlertTitle>Problems in these definitions</AlertTitle>
              <AlertDescription>
                <ul className="list-disc pl-4">
                  {(data.problems ?? []).map((p) => (
                    <li key={`${p.path}:${p.field}:${p.message}`}>
                      <code className="font-mono">{p.path}</code>
                      {p.field && <> · {p.field}</>}: {p.message}
                    </li>
                  ))}
                </ul>
              </AlertDescription>
            </Alert>
          )}
          <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {(data.items ?? []).map((item) => (
              <li key={`${item.kind}/${item.name}`}>
                <Card className="h-full">
                  <CardHeader className="flex-1">
                    <Badge variant="secondary" className="w-fit">
                      {item.kind === "type" ? "Resource type" : "Blueprint"}
                      {item.version && ` · v${item.version}`}
                    </Badge>
                    <CardTitle>{item.title}</CardTitle>
                    {item.description && <CardDescription>{item.description}</CardDescription>}
                  </CardHeader>
                  <CardContent>
                    <Link
                      to={item.kind === "type" ? "/preview/$repository/types/$name" : "/preview/$repository/blueprints/$name"}
                      params={{ repository, name: item.name }}
                      search={search}
                      className={buttonVariants({ size: "sm", variant: "outline" })}
                      aria-label={`Try ${item.title}`}
                    >
                      <Eye /> Try the form
                    </Link>
                  </CardContent>
                </Card>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  )
}
