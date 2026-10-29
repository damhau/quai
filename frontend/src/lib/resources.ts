// How resources read in the UI.

/** openStates are the request states that hold a resource: one open request per target. */
export const openStates = ["planned", "pending_approval", "approved", "stale", "merging", "applying"]

export const resourceStatus: Record<string, { label: string; tone: "good" | "waiting" | "bad" }> = {
  conforming: { label: "Conforming", tone: "good" },
  "non-conforming": { label: "Non-conforming", tone: "waiting" },
  unparseable: { label: "Unparseable", tone: "bad" },
}

/** resourceStates are what a resource's runs say since its last change (docs/design/06). */
export const resourceStates: Record<string, { label: string; tone: "neutral" | "good" | "waiting" | "bad" }> = {
  in_repo: { label: "In repo", tone: "neutral" },
  pending_change: { label: "Pending change", tone: "waiting" },
  planning: { label: "Planning", tone: "waiting" },
  applying: { label: "Applying", tone: "waiting" },
  applied: { label: "Applied", tone: "good" },
  failed: { label: "Failed", tone: "bad" },
  drifted: { label: "Drifted", tone: "bad" },
}

/** resourcePath is the page of a resource, from its id: <repository>/<type>/<key>. */
export function resourceParams(id: string) {
  const [repository = "", type = "", ...key] = id.split("/")
  return { repository, type, _splat: key.join("/") }
}
