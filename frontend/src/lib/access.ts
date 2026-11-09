import type { AccessUserView } from "@/api/generated/types.gen"

// The access report (docs/design/08-identity-access-tenancy.md#directory): every person who signed
// in, with the teams and roles their groups give them.

/** accessExportURL downloads the access report as CSV. */
export const accessExportURL = "/api/v1/access-report/export"

/** searchText is what the filter looks in: name, email, state, groups, teams, roles and tokens. */
function searchText(u: AccessUserView): string {
  const tokens = u.tokens ?? []
  const roles = [...(u.roles ?? []), ...tokens.flatMap((t) => t.roles ?? [])].flatMap((r) => [r.role, r.scope, `${r.role}@${r.scope}`])
  const names = tokens.flatMap((t) => [t.name, ...(t.scopes ?? []), ...(t.groups ?? [])])
  return [u.name, u.email ?? "", u.state, ...(u.groups ?? []), ...(u.teams ?? []), ...roles, ...names].join("\n").toLowerCase()
}

/** matches says whether a person has every word of q in their name, email, state, groups, teams, roles or tokens. */
export function matches(u: AccessUserView, q: string): boolean {
  const text = searchText(u)
  return q
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((w) => text.includes(w))
}
