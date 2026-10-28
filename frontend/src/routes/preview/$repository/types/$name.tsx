import { createFileRoute } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { previewDefinitionsOptions, previewTypeOptions } from "@/api/generated/@tanstack/react-query.gen"
import { TypeRequest } from "@/components/requests/type-request"
import { previewLabel, previewSearch } from "@/lib/preview"

export const Route = createFileRoute("/preview/$repository/types/$name")({
  validateSearch: previewSearch,
  loaderDeps: ({ search }) => search,
  loader: ({ context, params, deps }) =>
    Promise.all([
      context.queryClient.ensureQueryData(previewTypeOptions({ path: params, query: deps })),
      context.queryClient.ensureQueryData(previewDefinitionsOptions({ path: { repository: params.repository }, query: deps })),
    ]),
  component: Page,
})

function Page() {
  const { repository, name } = Route.useParams()
  const search = Route.useSearch()
  const { data: t } = useSuspenseQuery(previewTypeOptions({ path: { repository, name }, query: search }))
  const { data: p } = useSuspenseQuery(previewDefinitionsOptions({ path: { repository }, query: search }))
  return <TypeRequest key={`${repository}/${name}/${p.commit}`} repository={repository} name={name} t={t} preview={{ label: previewLabel(search), commit: p.commit }} />
}
