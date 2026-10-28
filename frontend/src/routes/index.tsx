import { useMemo, useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { Search, ShieldAlert } from "lucide-react"
import { listCatalogOptions } from "@/api/generated/@tanstack/react-query.gen"
import type { CatalogItem } from "@/api/generated/types.gen"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import { Card, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/")({
  component: Catalog,
})

const category = (i: CatalogItem) => i.category || "Other"

function matches(i: CatalogItem, q: string) {
  const text = `${i.title} ${i.description ?? ""} ${(i.tags ?? []).join(" ")}`.toLowerCase()
  return q.toLowerCase().split(/\s+/).every((w) => text.includes(w))
}

function Catalog() {
  const { data, isPending, error } = useQuery(listCatalogOptions())
  const items = useMemo(() => data ?? [], [data])
  const [q, setQ] = useState("")
  const [cat, setCat] = useState("All")
  const categories = useMemo(() => ["All", ...new Set(items.map(category))], [items])
  const shown = items.filter((i) => (cat === "All" || category(i) === cat) && matches(i, q))
  return (
    <section aria-labelledby="catalog-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="catalog-title" className="text-2xl font-semibold tracking-tight">
          Catalog
        </h1>
        <p className="text-muted-foreground">Pick an item and fill in the form: Quai turns your request into a reviewed change in Git.</p>
      </header>
      <div className="flex flex-wrap items-center gap-3">
        <div className="relative w-full max-w-xs">
          <Search className="text-muted-foreground absolute top-1/2 left-2.5 size-4 -translate-y-1/2" aria-hidden />
          <Input className="pl-8" placeholder="Search the catalog" aria-label="Search the catalog" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        {categories.length > 2 && (
          <ToggleGroup type="single" variant="outline" size="sm" aria-label="Category" value={cat} onValueChange={(c) => c && setCat(c)}>
            {categories.map((c) => (
              <ToggleGroupItem key={c} value={c} className="px-3">
                {c}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
        )}
      </div>
      {isPending && <Skeleton className="h-32" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>The catalog cannot be loaded: {problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {!isPending && !error && shown.length === 0 && <p className="text-muted-foreground">{items.length ? "Nothing matches." : "Nothing to request yet."}</p>}
      <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {shown.map((item) => (
          <li key={`${item.repository}/${item.kind}/${item.name}`}>
            <Card className="h-full">
              <CardHeader className="flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant="secondary">{category(item)}</Badge>
                  {(item.trust ?? []).length > 0 && (
                    <Badge variant="destructive" title={(item.trust ?? []).join("\n")}>
                      <ShieldAlert aria-hidden /> Unreviewed definitions
                    </Badge>
                  )}
                  <span className="text-muted-foreground text-xs">
                    {item.kind === "type" ? "Resource type" : "Blueprint"}
                    {item.version && ` · v${item.version}`}
                  </span>
                </div>
                <CardTitle>{item.title}</CardTitle>
                {item.description && <CardDescription>{item.description}</CardDescription>}
              </CardHeader>
              <CardFooter className="justify-between gap-2">
                <span className="text-muted-foreground truncate text-xs">{item.repository}</span>
                <Link
                  to={item.kind === "type" ? "/request/$repository/types/$name" : "/request/$repository/blueprints/$name"}
                  params={{ repository: item.repository, name: item.name }}
                  className={buttonVariants({ size: "sm" })}
                  aria-label={`Request ${item.title}`}
                >
                  Request
                </Link>
              </CardFooter>
            </Card>
          </li>
        ))}
      </ul>
    </section>
  )
}
