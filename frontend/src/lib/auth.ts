import type { QueryClient } from "@tanstack/react-query"
import { getMeQueryKey } from "@/api/generated/@tanstack/react-query.gen"
import type { MeOutputBody } from "@/api/generated/types.gen"

// The /auth endpoints are browser flows (redirects and the development switcher), outside the
// OpenAPI document, so they are called here directly.

export type DevUser = { Name: string; Groups: string[] | null }

/** isUnauthorized reports whether an API error is a 401 problem: nobody is signed in. */
export function isUnauthorized(error: unknown): boolean {
  return typeof error === "object" && error !== null && "status" in error && error.status === 401
}

/** localPath keeps a redirect target only when it is a path on this site. */
export function localPath(next: unknown): string {
  return typeof next === "string" && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\") ? next : "/"
}

function url(path: string) {
  return new URL(path, window.location.origin)
}

export async function listDevUsers(): Promise<DevUser[]> {
  const res = await fetch(url("/auth/dev/users"))
  if (!res.ok) throw new Error(`development users: ${res.status}`)
  return (await res.json()) as DevUser[]
}

export async function devLogin(name: string): Promise<void> {
  const res = await fetch(url("/auth/dev/login"), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  })
  if (!res.ok) throw new Error(`sign in as ${name}: ${res.status}`)
}

export async function logout(queryClient: QueryClient): Promise<void> {
  const me = queryClient.getQueryData<MeOutputBody>(getMeQueryKey())
  await fetch(url("/auth/logout"), { method: "POST", headers: { "X-CSRF-Token": me?.csrfToken ?? "" } })
  queryClient.clear()
}

/** oidcLoginURL is where the browser starts the identity provider's login, to come back to next. */
export function oidcLoginURL(next: string): string {
  return `/auth/login?next=${encodeURIComponent(localPath(next))}`
}

/** oidcLogin sends the browser to the identity provider. */
export function oidcLogin(next: string) {
  window.location.assign(oidcLoginURL(next))
}
