import { render } from "@testing-library/react"
import { QueryClientProvider } from "@tanstack/react-query"
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router"
import { configureClient } from "@/api/client"
import { makeQueryClient } from "@/lib/query-client"
import { createAppRouter } from "@/router"

/** renderApp renders the whole app at path, against the fake backend in ./server. */
export function renderApp(path: string) {
  const queryClient = makeQueryClient()
  configureClient(queryClient)
  const router = createAppRouter(queryClient, createMemoryHistory({ initialEntries: [path] }))
  const view = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  return { ...view, router, queryClient }
}
