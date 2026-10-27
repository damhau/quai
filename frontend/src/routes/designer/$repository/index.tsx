import { useState } from "react"
import { createFileRoute } from "@tanstack/react-router"
import { newBlueprint } from "@/components/designer/blueprint"
import { Designer } from "@/components/designer/designer"

export const Route = createFileRoute("/designer/$repository/")({
  component: NewBlueprint,
})

function NewBlueprint() {
  const { repository } = Route.useParams()
  const [initial] = useState(newBlueprint)
  return <Designer key={repository} repository={repository} initial={initial} isNew />
}
