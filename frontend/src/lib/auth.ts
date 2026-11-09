import type { QueryClient } from "@tanstack/react-query"
import { getMeOptions, getMeQueryKey } from "@/api/generated/@tanstack/react-query.gen"
import type { MeOutputBody } from "@/api/generated/types.gen"

// The /auth endpoints are browser flows (redirects and the development switcher), outside the
// OpenAPI document, so they are called here directly.

export type DevUser = { Name: string; Groups: string[] | null }

/** isUnauthorized reports whether an API error is a 401 problem: nobody is signed in. */
export function isUnauthorized(error: unknown): boolean {
  return typeof error === "object" && error !== null && "status" in error && error.status === 401
}

/** maxLocalPath is the longest redirect target kept, as on the server. */
const maxLocalPath = 2048

/** hasControl reports whether s holds a control character, which browsers drop from URLs. */
function hasControl(s: string): boolean {
  for (const c of s) {
    const n = c.codePointAt(0) ?? 0
    if (n < 0x20 || (n >= 0x7f && n < 0xa0)) return true
  }
  return false
}

/**
 * localPath keeps a redirect target only when it is a path on this site, as the server does when
 * a login starts: it starts with a single /, holds no backslash and no control character, however
 * many times it is percent-decoded (/%2F/evil.example.com is //evil.example.com to some). Anything
 * else is /.
 */
export function localPath(next: unknown): string {
  if (typeof next !== "string" || next.length > maxLocalPath || !next.startsWith("/")) return "/"
  let s = next
  for (let i = 0; i < 4; i++) {
    if (s.startsWith("//") || s.includes("\\") || hasControl(s)) return "/"
    let decoded: string
    try {
      decoded = decodeURIComponent(s)
    } catch {
      return "/"
    }
    if (decoded === s) return next
    s = decoded
  }
  return "/"
}

function url(path: string) {
  return new URL(path, window.location.origin)
}

export async function listDevUsers(): Promise<DevUser[]> {
  const res = await fetch(url("/auth/dev/users"))
  if (!res.ok) throw new Error(`development users: ${res.status}`)
  return (await res.json()) as DevUser[]
}

/**
 * devLogin switches to a user of the development switcher. With a session already, the server
 * wants that session's CSRF token, as for any request that changes something.
 */
export async function devLogin(queryClient: QueryClient, name: string): Promise<void> {
  const me = await queryClient.fetchQuery(getMeOptions()).catch(() => undefined)
  const res = await fetch(url("/auth/dev/login"), {
    method: "POST",
    headers: { "Content-Type": "application/json", ...(me?.csrfToken && { "X-CSRF-Token": me.csrfToken }) },
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

/** browser leaves this page for another; tests replace assign, since jsdom does not navigate. */
export const browser = { assign: (href: string) => window.location.assign(href) }

/** oidcLogin sends the browser to the identity provider. */
export function oidcLogin(next: string) {
  browser.assign(oidcLoginURL(next))
}

/**
 * stepUpLoginURL is where the browser signs in again, for an action that needs a recent sign-in
 * (step-up): the identity provider authenticates the person again, then the browser comes back to
 * next.
 */
export function stepUpLoginURL(next: string): string {
  return `/auth/login?stepUp=1&next=${encodeURIComponent(localPath(next))}`
}

/**
 * stepUp signs the person in again for an action that needs a recent sign-in. With the identity
 * provider the browser leaves, and comes back to next; with the development switcher, the same
 * person signs in again here, and everything read with the old session is read again.
 */
export async function stepUp(queryClient: QueryClient, auth: string | undefined, name: string, next: string): Promise<void> {
  if (auth === "oidc") {
    browser.assign(stepUpLoginURL(next))
    return
  }
  await devLogin(queryClient, name)
  await queryClient.invalidateQueries()
}
