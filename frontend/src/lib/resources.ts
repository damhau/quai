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

type Me = { name: string; email?: string; groups: string[] | null; teams: string[] | null }

/**
 * ownedBy says whether me owns r: as its owner, in its owning team (team:<name>, the teams auth.teams
 * maps me's groups to), or as one of its code owners (docs/design/02).
 */
export function ownedBy(r: { owner?: string; owners?: string[] | null }, me: Me | undefined) {
  if (!me) return false
  const names = (entry: string) => {
    const [kind, v = ""] = entry.split(/:(.*)/s)
    if (!v) return false
    if (kind === "user") return v === me.name
    if (kind === "group") return (me.groups ?? []).includes(v)
    if (kind === "team") return (me.teams ?? []).includes(v)
    if (kind === "email") return !!me.email && v.toLowerCase() === me.email.toLowerCase()
    return false
  }
  return r.owner === me.name || (!!r.owner?.startsWith("team:") && names(r.owner)) || (r.owners ?? []).some(names)
}

/** ownerText is who owns r, as a list shows it. */
export function ownerText(r: { owner?: string; owners?: string[] | null }) {
  return r.owner ?? (r.owners?.length ? r.owners.join(", ") : undefined)
}

/** resourcePath is the page of a resource, from its id: <repository>/<type>/<key>. */
export function resourceParams(id: string) {
  const [repository = "", type = "", ...key] = id.split("/")
  return { repository, type, _splat: key.join("/") }
}
