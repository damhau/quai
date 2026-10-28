import { createFileRoute } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { getTypeOptions } from "@/api/generated/@tanstack/react-query.gen"
import { TypeRequest } from "@/components/requests/type-request"

export const Route = createFileRoute("/request/$repository/types/$name")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getTypeOptions({ path: params })),
  component: Page,
})

function Page() {
  const { repository, name } = Route.useParams()
  const { data: t } = useSuspenseQuery(getTypeOptions({ path: { repository, name } }))
  return <TypeRequest key={`${repository}/${name}`} repository={repository} name={name} t={t} />
}
