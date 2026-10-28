/** PreviewSearch names what a preview shows: a branch, or a pull request by its ID. */
export type PreviewSearch = { branch?: string; pullRequest?: string }

/** previewSearch reads a preview's search parameters, keeping only the one that is set. */
export function previewSearch(search: Record<string, unknown>): PreviewSearch {
  const str = (v: unknown) => (typeof v === "string" && v.trim() !== "" ? v.trim() : typeof v === "number" ? String(v) : undefined)
  const pullRequest = str(search.pullRequest)
  return pullRequest ? { pullRequest } : { branch: str(search.branch) }
}

/** previewLabel names a preview for people: "the branch x" or "pull request #12". */
export function previewLabel(s: PreviewSearch): string {
  return s.pullRequest ? `pull request #${s.pullRequest}` : `the branch ${s.branch}`
}
