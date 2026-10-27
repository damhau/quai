// How resources read in the UI.

/** openStates are the request states that hold a resource: one open request per target. */
export const openStates = ["planned", "pending_approval", "approved", "stale", "merging", "applying"]

export const resourceStatus: Record<string, { label: string; tone: "good" | "waiting" | "bad" }> = {
  conforming: { label: "Conforming", tone: "good" },
  "non-conforming": { label: "Non-conforming", tone: "waiting" },
  unparseable: { label: "Unparseable", tone: "bad" },
}

/** resourcePath is the page of a resource, from its id: <repository>/<type>/<key>. */
export function resourceParams(id: string) {
  const [repository = "", type = "", ...key] = id.split("/")
  return { repository, type, _splat: key.join("/") }
}
