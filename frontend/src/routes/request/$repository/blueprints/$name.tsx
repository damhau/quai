import { createFileRoute } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { getBlueprintOptions } from "@/api/generated/@tanstack/react-query.gen"
import { BlueprintRequest } from "@/components/requests/blueprint-request"

export const Route = createFileRoute("/request/$repository/blueprints/$name")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getBlueprintOptions({ path: params })),
  component: Page,
})

function Page() {
  const { repository, name } = Route.useParams()
  const { data: b } = useSuspenseQuery(getBlueprintOptions({ path: { repository, name } }))
  return <BlueprintRequest key={`${repository}/${name}`} repository={repository} name={name} b={b} />
}
