import { keepPreviousData, useQuery } from "@tanstack/react-query"
import type { PlanView } from "@/api/generated/types.gen"
import { useDebounced } from "@/lib/use-debounced"

/**
 * useLivePlan asks the server to plan a request 300 ms after its form stops changing, keeping the
 * last plan on screen meanwhile. settled is the body as last planned, as JSON.
 */
export function useLivePlan<B>(key: string, body: B, plan: (body: B, signal: AbortSignal) => Promise<PlanView>, enabled = true) {
  const settled = useDebounced(JSON.stringify(body), 300)
  const query = useQuery({
    queryKey: [key, settled],
    queryFn: ({ signal }) => plan(JSON.parse(settled) as B, signal),
    enabled,
    placeholderData: keepPreviousData,
  })
  return { plan: query, settled }
}
