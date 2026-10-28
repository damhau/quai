import { createFileRoute } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { previewDefinitionsOptions, previewBlueprintOptions } from "@/api/generated/@tanstack/react-query.gen"
import { BlueprintRequest } from "@/components/requests/blueprint-request"
import { previewLabel, previewSearch } from "@/lib/preview"

export const Route = createFileRoute("/preview/$repository/blueprints/$name")({
  validateSearch: previewSearch,
  loaderDeps: ({ search }) => search,
  loader: ({ context, params, deps }) =>
    Promise.all([
      context.queryClient.ensureQueryData(previewBlueprintOptions({ path: params, query: deps })),
      context.queryClient.ensureQueryData(previewDefinitionsOptions({ path: { repository: params.repository }, query: deps })),
    ]),
  component: Page,
})

function Page() {
  const { repository, name } = Route.useParams()
  const search = Route.useSearch()
  const { data: b } = useSuspenseQuery(previewBlueprintOptions({ path: { repository, name }, query: search }))
  const { data: p } = useSuspenseQuery(previewDefinitionsOptions({ path: { repository }, query: search }))
  return <BlueprintRequest key={`${repository}/${name}/${p.commit}`} repository={repository} name={name} b={b} preview={{ label: previewLabel(search), commit: p.commit }} />
}
