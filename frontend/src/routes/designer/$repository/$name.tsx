import { createFileRoute } from "@tanstack/react-router"
import { useSuspenseQuery } from "@tanstack/react-query"
import { getBlueprintOptions } from "@/api/generated/@tanstack/react-query.gen"
import { newBlueprint } from "@/components/designer/blueprint"
import { Designer } from "@/components/designer/designer"

export const Route = createFileRoute("/designer/$repository/$name")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getBlueprintOptions({ path: params })),
  component: EditBlueprint,
})

function EditBlueprint() {
  const { repository, name } = Route.useParams()
  const { data: b } = useSuspenseQuery(getBlueprintOptions({ path: { repository, name } }))
  return <Designer key={`${repository}/${name}`} repository={repository} initial={b.definition ?? newBlueprint()} isNew={false} />
}
