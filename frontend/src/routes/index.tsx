import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { listCatalogOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Badge } from "@/components/ui/badge"
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"

export const Route = createFileRoute("/")({
  component: Catalog,
})

function Catalog() {
  const { data: items, isPending, error } = useQuery(listCatalogOptions())
  return (
    <section aria-labelledby="catalog-title" className="grid gap-4">
      <h1 id="catalog-title" className="text-2xl font-semibold tracking-tight">
        Catalog
      </h1>
      {isPending && <Skeleton className="h-24" />}
      {error && <p role="alert">The catalog cannot be loaded.</p>}
      <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {(items ?? []).map((item) => (
          <li key={`${item.repository}/${item.kind}/${item.name}`}>
            <Link to="/" className="block">
              <Card className="h-full">
                <CardHeader>
                  <div className="flex items-center justify-between gap-2">
                    <CardTitle>{item.title}</CardTitle>
                    <Badge variant="secondary">{item.kind === "type" ? "Resource" : "Blueprint"}</Badge>
                  </div>
                  {item.description && <CardDescription>{item.description}</CardDescription>}
                  <p className="text-muted-foreground text-xs">
                    {item.repository} · {item.category}
                  </p>
                </CardHeader>
              </Card>
            </Link>
          </li>
        ))}
      </ul>
    </section>
  )
}
