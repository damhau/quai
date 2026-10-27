import { createRouter, type RouterHistory } from "@tanstack/react-router"
import type { QueryClient } from "@tanstack/react-query"
import { RouteError } from "@/components/route-error"
import { Skeleton } from "@/components/ui/skeleton"
import { routeTree } from "./routeTree.gen"

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  return createRouter({
    routeTree,
    context: { queryClient },
    history,
    defaultPreload: "intent",
    // Loaders fill the query cache; the cache decides when data is stale.
    defaultPreloadStaleTime: 0,
    defaultPendingComponent: () => <Skeleton className="h-64" />,
    defaultErrorComponent: RouteError,
    defaultNotFoundComponent: () => <p className="text-muted-foreground">There is no such page.</p>,
  })
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
