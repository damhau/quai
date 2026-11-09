import { downloadEvidenceArchive } from "@/api/generated/sdk.gen"
import type { MeOutputBody } from "@/api/generated/types.gen"

// Evidence bundles (docs/design/09-security-and-compliance.md#evidence-bundle): a request's
// download, in JSON to verify offline and as a PDF summary to read, and the auditors' sample of
// requests, downloaded as one archive.

/**
 * canSampleEvidence says whether someone may list the requests of a sample and download their
 * archive: auditors, of the whole platform or of a repository, and platform admins.
 */
export function canSampleEvidence(me: MeOutputBody | undefined): boolean {
  return (me?.roles ?? []).some((r) => r.role === "auditor" || r.role === "platform-admin")
}

/**
 * sampledRepositories returns the repositories someone may sample: undefined for all of them (an
 * auditor of the platform, a platform admin), else those they audit.
 */
export function sampledRepositories(me: MeOutputBody | undefined): string[] | undefined {
  const roles = me?.roles ?? []
  if (roles.some((r) => (r.role === "auditor" || r.role === "platform-admin") && r.scope === "global")) return undefined
  return roles.filter((r) => r.role === "auditor" && r.scope.startsWith("repository:")).map((r) => r.scope.slice("repository:".length))
}

/** evidenceURL is a request's bundle: its JSON, or its PDF summary. */
export const evidenceURL = (id: string, format: "json" | "pdf") => `/api/v1/requests/${encodeURIComponent(id)}/evidence${format === "pdf" ? "?format=pdf" : ""}`

/** maxArchive is how many requests one archive holds at most, as the server bounds it. */
export const maxArchive = 50

/** archiveName is the name the server gave an archive, or a default: never a name from elsewhere. */
export function archiveName(disposition: string | null): string {
  const m = /filename="(quai-evidence-[0-9A-Z]+\.zip)"/.exec(disposition ?? "")
  return m?.[1] ?? "quai-evidence.zip"
}

/** saveArchive downloads the bundles of requests as one archive, and saves it. */
export async function saveArchive(requests: string[]): Promise<void> {
  const { data, response } = await downloadEvidenceArchive({ body: { requests }, parseAs: "blob", throwOnError: true })
  const url = URL.createObjectURL(data as Blob)
  const a = document.createElement("a")
  a.href = url
  a.download = archiveName(response.headers.get("Content-Disposition"))
  document.body.append(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
