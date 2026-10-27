import { createFileRoute } from "@tanstack/react-router"

export const Route = createFileRoute("/inventory/")({
  component: InventoryPage,
})

function InventoryPage() {
  return (
    <section className="grid gap-2">
      <h1 className="text-2xl font-semibold tracking-tight">Inventory</h1>
      <p className="text-muted-foreground">This page arrives with P0-23.</p>
    </section>
  )
}
