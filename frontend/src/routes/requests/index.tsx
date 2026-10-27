import { createFileRoute } from "@tanstack/react-router"

export const Route = createFileRoute("/requests/")({
  component: RequestsPage,
})

function RequestsPage() {
  return (
    <section className="grid gap-2">
      <h1 className="text-2xl font-semibold tracking-tight">Requests</h1>
      <p className="text-muted-foreground">This page arrives with P0-22.</p>
    </section>
  )
}
