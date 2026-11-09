import { http, HttpResponse } from "msw"
import type { EvidenceRequestView } from "@/api/generated/types.gen"

// A fake of the auditors' evidence sample: three requests of the period, the queries it was asked
// and the archives it made, with the CSRF token each came with.

const row = (number: number, r: Partial<EvidenceRequestView>): EvidenceRequestView => ({
  id: `${number}`.padStart(8, "0") + "-0000-4000-8000-000000000000",
  number,
  repository: "infra-live",
  target: `infra-live/vm/prod/web0${number}`,
  kind: "create",
  state: "applied",
  requester: "bob",
  breakGlass: false,
  createdAt: `2026-11-0${number}T10:00:00.000Z`,
  ...r,
})

export const sampled: EvidenceRequestView[] = [row(1, {}), row(2, { state: "rejected" }), row(3, { breakGlass: true, requester: "carol" })]

export const evidence = {
  queries: [] as URLSearchParams[],
  archives: [] as { requests: string[]; csrf: string | null }[],
}

export function resetEvidence() {
  evidence.queries = []
  evidence.archives = []
}

export const evidenceHandlers = [
  http.get("*/api/v1/evidence/requests", ({ request }) => {
    const q = new URL(request.url).searchParams
    evidence.queries.push(q)
    const requests = sampled.filter(
      (r) =>
        (!q.get("state") || r.state === q.get("state")) &&
        (!q.get("repository") || r.repository === q.get("repository")) &&
        (q.get("breakGlass") !== "true" || r.breakGlass),
    )
    return HttpResponse.json({ requests, truncated: false })
  }),
  http.post("*/api/v1/evidence/archive", async ({ request }) => {
    const { requests } = (await request.json()) as { requests: string[] }
    evidence.archives.push({ requests, csrf: request.headers.get("X-CSRF-Token") })
    return new HttpResponse(new Uint8Array([0x50, 0x4b, 5, 6]), {
      headers: { "Content-Type": "application/zip", "Content-Disposition": `attachment; filename="quai-evidence-20261109T120000Z.zip"` },
    })
  }),
]
