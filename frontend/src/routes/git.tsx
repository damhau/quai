import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { useQuery } from "@tanstack/react-query"
import { listRepositoriesOptions, repositoryCommitOptions, repositoryLogOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ago, shortSha } from "@/lib/format"
import { problemMessage } from "@/lib/problems"
import { cn } from "@/lib/utils"

type Search = { repository?: string; sha?: string }

export const Route = createFileRoute("/git")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    repository: typeof s.repository === "string" ? s.repository : undefined,
    sha: typeof s.sha === "string" && /^[0-9a-f]{7,64}$/.test(s.sha) ? s.sha : undefined,
  }),
  component: GitLog,
})

function GitLog() {
  const search = Route.useSearch()
  const navigate = useNavigate({ from: "/git" })
  const { data: repos, error: reposError } = useQuery(listRepositoriesOptions())
  const repository = search.repository ?? repos?.[0]?.name
  const log = useQuery({ ...repositoryLogOptions({ path: { repository: repository ?? "" }, query: { limit: 100 } }), enabled: !!repository, refetchInterval: 30_000 })
  const commits = log.data ?? []
  const error = reposError ?? log.error
  return (
    <section aria-labelledby="git-title" className="grid gap-6">
      <header className="grid gap-1">
        <h1 id="git-title" className="text-2xl font-semibold tracking-tight">
          Git log
        </h1>
        <p className="text-muted-foreground">What landed on each repository's branch, through Quai or not.</p>
      </header>
      {(repos?.length ?? 0) > 1 && (
        <div className="flex items-center gap-2">
          <Label htmlFor="repository">Repository</Label>
          <NativeSelect id="repository" value={repository} onChange={(e) => void navigate({ search: { repository: e.target.value } })}>
            {(repos ?? []).map((r) => (
              <NativeSelectOption key={r.name} value={r.name}>
                {r.title || r.name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
      )}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {log.isPending && repository && <Skeleton className="h-64" />}
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
        <ol className="grid gap-1" aria-label="Commits">
          {commits.map((c) => (
            <li key={c.sha}>
              <Link
                to="/git"
                search={{ repository, sha: c.sha }}
                aria-current={search.sha === c.sha ? "true" : undefined}
                className={cn("hover:bg-accent block rounded-md px-3 py-2", search.sha === c.sha && "bg-accent")}
              >
                <span className="block truncate text-sm font-medium">{c.subject}</span>
                <span className="text-muted-foreground text-xs">
                  <code className="font-mono">{shortSha(c.sha)}</code> · {c.author} · {ago(c.date)}
                </span>
              </Link>
            </li>
          ))}
        </ol>
        {repository && search.sha && <Commit repository={repository} sha={search.sha} />}
      </div>
    </section>
  )
}

function lineClass(line: string, inPatch: boolean) {
  if (!inPatch) return ""
  if (line.startsWith("+") && !line.startsWith("+++")) return "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
  if (line.startsWith("-") && !line.startsWith("---")) return "bg-red-500/10 text-red-700 dark:text-red-300"
  if (line.startsWith("@@") || line.startsWith("diff --git")) return "text-muted-foreground"
  return ""
}

function Commit({ repository, sha }: { repository: string; sha: string }) {
  const { data, isPending, error } = useQuery(repositoryCommitOptions({ path: { repository, sha } }))
  const lines = (data?.text ?? "").split("\n")
  const patchAt = lines.findIndex((l) => l.startsWith("diff --git"))
  return (
    <Card className="lg:sticky lg:top-6">
      <CardHeader>
        <CardTitle className="font-mono text-sm">{shortSha(sha)}</CardTitle>
      </CardHeader>
      <CardContent>
        {isPending && <Skeleton className="h-40" />}
        {error && <p className="text-destructive text-sm">{problemMessage(error)}</p>}
        {data && (
          <pre className="bg-muted/40 max-h-[70vh] overflow-auto rounded-md py-2 font-mono text-xs leading-5" aria-label={`Commit ${shortSha(sha)}`}>
            {lines.map((l, i) => (
              <div key={i} className={cn("px-3 whitespace-pre", lineClass(l, patchAt >= 0 && i >= patchAt))}>
                {l || " "}
              </div>
            ))}
          </pre>
        )}
      </CardContent>
    </Card>
  )
}
