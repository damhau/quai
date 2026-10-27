import { QueryClient } from "@tanstack/react-query"

// One client for the app: server state lives here, never in a store.
export function makeQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { staleTime: 30_000, refetchOnWindowFocus: false, retry: false } },
  })
}

export const queryClient = makeQueryClient()
