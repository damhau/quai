import type { EntitlementsView } from "@/api/generated/types.gen"

/**
 * Entitlements say who sees a catalog item and who may request it, environment by environment
 * (docs/design/08-identity-access-tenancy.md#entitlements). The server enforces them; the pages only say
 * what they are.
 */

const list = (entries?: string[] | null) => (entries?.length ? entries.join(", ") : "everyone")

/** entitlementLines describes the lists, one line each. */
export function entitlementLines(e: EntitlementsView): string[] {
  return [
    `Visible to ${list(e.visibility)}`,
    `Requested by ${list(e.requesters)}`,
    ...(e.environments ?? []).map((env) => `In ${env.environment}, only ${list(env.requesters)}`),
  ]
}

/** closedEnvironments are the environments the caller may not request the item in. */
export function closedEnvironments(e?: EntitlementsView | null): string[] {
  return (e?.environments ?? []).filter((env) => env.canRequest === false).map((env) => env.environment)
}
