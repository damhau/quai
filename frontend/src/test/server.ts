import { http, HttpResponse } from "msw"
import { setupServer } from "msw/node"

// A fake backend for the shell: who is signed in, and how this server signs people in.
export const state: { user: string | null; auth: "dev" | "oidc" | "none"; logouts: (string | null)[] } = {
  user: "bob",
  auth: "dev",
  logouts: [],
}

const users: Record<string, string[]> = { bob: ["developers"], alice: ["platform-admins"] }

export const server = setupServer(
  http.get("*/api/v1/meta", () => HttpResponse.json({ version: "test", env: "development", auth: state.auth })),
  http.get("*/api/v1/me", () =>
    state.user
      ? HttpResponse.json({
          name: state.user,
          groups: users[state.user],
          roles: [{ role: "requester", scope: "global" }],
          csrfToken: `csrf-${state.user}`,
        })
      : HttpResponse.json({ title: "Unauthorized", status: 401, detail: "not signed in" }, { status: 401 }),
  ),
  http.get("*/api/v1/catalog", () =>
    HttpResponse.json([
      { kind: "type", repository: "infra-live", name: "vm", title: "Virtual machine (Terraform)", category: "Compute", tags: [] },
      { kind: "blueprint", repository: "infra-live", name: "linux-vm", title: "Linux virtual machine", category: "Compute", tags: [] },
    ]),
  ),
  http.get("*/auth/dev/users", () => HttpResponse.json(Object.entries(users).map(([Name, Groups]) => ({ Name, Groups })))),
  http.post("*/auth/dev/login", async ({ request }) => {
    const { name } = (await request.json()) as { name: string }
    state.user = name
    return new HttpResponse(null, { status: 204 })
  }),
  http.post("*/auth/logout", ({ request }) => {
    state.logouts.push(request.headers.get("X-CSRF-Token"))
    state.user = null
    return new HttpResponse(null, { status: 204 })
  }),
)
