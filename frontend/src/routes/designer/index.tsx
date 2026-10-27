import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { Pencil, Plus } from "lucide-react"
import { listCatalogOptions, listRepositoriesOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/designer/")({
  component: DesignerIndex,
})

function DesignerIndex() {
  const { data: repos, isPending, error } = useQuery(listRepositoriesOptions())
  const { data: catalog } = useQuery(listCatalogOptions())
  return (
    <section aria-labelledby="designer-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="designer-title" className="text-2xl font-semibold tracking-tight">
          Designer
        </h1>
        <p className="text-muted-foreground">Compose blueprints from the types of a repository. Publishing commits the blueprint to that repository.</p>
      </header>
      {isPending && <Skeleton className="h-32" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {(repos ?? []).map((repo) => {
        const blueprints = (catalog ?? []).filter((i) => i.kind === "blueprint" && i.repository === repo.name)
        return (
          <Card key={repo.name}>
            <CardHeader className="flex flex-row items-center justify-between gap-4">
              <CardTitle>{repo.title || repo.name}</CardTitle>
              <Link to="/designer/$repository" params={{ repository: repo.name }} className={buttonVariants({ size: "sm", variant: "outline" })}>
                <Plus /> New blueprint
              </Link>
            </CardHeader>
            <CardContent>
              {blueprints.length ? (
                <ul className="grid gap-2">
                  {blueprints.map((b) => (
                    <li key={b.name} className="flex items-center justify-between gap-4">
                      <div>
                        <span className="font-medium">{b.title}</span>
                        {b.description && <p className="text-muted-foreground text-sm">{b.description}</p>}
                      </div>
                      <Link
                        to="/designer/$repository/$name"
                        params={{ repository: repo.name, name: b.name }}
                        className={buttonVariants({ size: "sm", variant: "ghost" })}
                        aria-label={`Edit ${b.title}`}
                      >
                        <Pencil /> Edit
                      </Link>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-muted-foreground text-sm">No blueprint in this repository yet.</p>
              )}
            </CardContent>
          </Card>
        )
      })}
    </section>
  )
}
