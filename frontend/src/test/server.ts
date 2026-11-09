import { http, HttpResponse } from "msw"
import { setupServer } from "msw/node"
import { accessHandlers, resetAccess } from "./access"
import { auditHandlers, resetAudit } from "./audit"
import { catalogHandlers } from "./catalog"
import { delegationHandlers, resetDelegations } from "./delegations"
import { evidenceHandlers, resetEvidence } from "./evidence"
import { gitHandlers, published } from "./git"
import { inventoryHandlers } from "./inventory"
import { repositoryHandlers, resetRepositories } from "./repositories"
import { preferencesHandlers, resetPreferences } from "./preferences"
import { pendingVM, requestHandlers, requests } from "./requests"

// A fake backend for the shell: who is signed in, and how this server signs people in; whether
// their sign-in is too old for a step-up (stale), and whether changing repositories needs one.
export const state: { user: string | null; auth: "dev" | "oidc" | "none"; logouts: (string | null)[]; stale: boolean; adminStepUp: boolean } = {
  user: "bob",
  auth: "dev",
  logouts: [],
  stale: false,
  adminStepUp: false,
}

function stepUpView() {
  return state.stale
    ? { satisfied: false, reason: "you authenticated more than 5m0s ago; sign in again", maxAge: 300, admin: state.adminStepUp }
    : { satisfied: true, until: "2026-10-27T10:05:00Z", maxAge: 300, admin: state.adminStepUp }
}

const users: Record<string, string[]> = { bob: ["developers"], alice: ["platform-admins"], dana: ["auditors"], dave: ["oncall"], rita: ["live-auditors"] }
// Verified emails from the identity provider: dana has none.
const emails: Record<string, string> = { bob: "bob@acme.ch", alice: "alice@acme.ch", dave: "dave@acme.ch" }
const roles: Record<string, { role: string; scope: string }[]> = {
  alice: [{ role: "platform-admin", scope: "global" }],
  dana: [{ role: "auditor", scope: "global" }],
  rita: [{ role: "auditor", scope: "repository:infra-live" }],
  dave: [
    { role: "viewer", scope: "global" },
    { role: "break-glass", scope: "global" },
  ],
}

export function resetRequests() {
  requests.current = pendingVM()
  requests.links = []
  requests.breaks = []
  published.blueprints = []
  resetRepositories()
  resetAudit()
  resetAccess()
  resetPreferences()
  resetDelegations()
  resetEvidence()
}

export const server = setupServer(
  http.get("*/api/v1/meta", () => HttpResponse.json({ version: "test", env: "development", auth: state.auth })),
  http.get("*/api/v1/me", () =>
    state.user
      ? HttpResponse.json({
          name: state.user,
          email: emails[state.user],
          groups: users[state.user],
          teams: users[state.user], // no auth.teams: each group is the team of its name
          roles: roles[state.user] ?? [{ role: "requester", scope: "global" }],
          csrfToken: `csrf-${state.user}`,
          stepUp: stepUpView(),
        })
      : HttpResponse.json({ title: "Unauthorized", status: 401, detail: "not signed in" }, { status: 401 }),
  ),
  ...catalogHandlers,
  ...requestHandlers(
    () => state.user,
    () => !state.stale,
  ),
  ...inventoryHandlers(() => state.user),
  ...gitHandlers,
  ...repositoryHandlers,
  ...auditHandlers,
  ...evidenceHandlers,
  ...accessHandlers(() => (state.user ? (roles[state.user] ?? []) : null)),
  ...preferencesHandlers(() => state.user),
  ...delegationHandlers(
    () => state.user,
    () => !state.stale,
  ),
  http.get("*/auth/dev/users", () => HttpResponse.json(Object.entries(users).map(([Name, Groups]) => ({ Name, Groups })))),
  http.post("*/auth/dev/login", async ({ request }) => {
    // As the server's middleware: with a session, a state-changing request needs its CSRF token.
    if (state.user && request.headers.get("X-CSRF-Token") !== `csrf-${state.user}`)
      return HttpResponse.json({ status: 403, detail: "missing or wrong X-CSRF-Token header" }, { status: 403 })
    const { name } = (await request.json()) as { name: string }
    // Signing in again is a fresh authentication: it steps up.
    state.user = name
    state.stale = false
    return new HttpResponse(null, { status: 204 })
  }),
  http.post("*/auth/logout", ({ request }) => {
    state.logouts.push(request.headers.get("X-CSRF-Token"))
    state.user = null
    return new HttpResponse(null, { status: 204 })
  }),
)
