import { createFileRoute } from "@tanstack/react-router"

export const Route = createFileRoute("/designer/")({
  component: DesignerPage,
})

function DesignerPage() {
  return (
    <section className="grid gap-2">
      <h1 className="text-2xl font-semibold tracking-tight">Designer</h1>
      <p className="text-muted-foreground">This page arrives with P0-24.</p>
    </section>
  )
}
