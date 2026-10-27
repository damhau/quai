import { client } from "@/api/generated/client.gen"
import { getMeQueryKey } from "@/api/generated/@tanstack/react-query.gen"
import type { MeOutputBody } from "@/api/generated/types.gen"
import type { QueryClient } from "@tanstack/react-query"

const safe = new Set(["GET", "HEAD", "OPTIONS"])
let current: QueryClient | undefined
let installed = false

// configureClient points the generated client at this origin, where Vite (in development) or
// the server proxies /api, and adds the session's CSRF token, from the cached /api/v1/me, to
// requests that change something.
export function configureClient(queryClient: QueryClient) {
  current = queryClient
  client.setConfig({ baseUrl: window.location.origin, credentials: "same-origin" })
  if (installed) return
  installed = true
  client.interceptors.request.use((request) => {
    if (!safe.has(request.method)) {
      const me = current?.getQueryData<MeOutputBody>(getMeQueryKey())
      if (me?.csrfToken) request.headers.set("X-CSRF-Token", me.csrfToken)
    }
    return request
  })
}
