import { http, HttpResponse } from "msw"
import type { AccessReportView } from "@/api/generated/types.gen"

// A fake of the access report: its people, and how many times it was read.

const report = (): AccessReportView => ({
  generatedAt: "2026-11-09T08:00:00Z",
  users: [
    {
      name: "=HYPERLINK(\"https://evil.example.com\")",
      groups: ["@SUM(A1)"],
      teams: ["@SUM(A1)"],
      roles: [{ role: "viewer", scope: "global", groups: ["@SUM(A1)"] }],
      tokens: [],
      state: "active",
      lastLogin: "2026-11-08T07:00:00Z",
      authTime: "2026-11-08T06:59:00Z",
    },
    {
      name: "bob",
      email: "bob@acme.ch",
      subject: "sub-bob",
      groups: ["developers", "shop"],
      teams: ["developers", "shop"],
      roles: [
        { role: "operator", scope: "team:shop", groups: ["shop"] },
        { role: "requester", scope: "global", groups: ["developers"] },
      ],
      // A token from when bob was a platform admin: it still acts with that group.
      tokens: [
        {
          id: "7d0c6a8e-0000-4000-8000-000000000001",
          name: "ci",
          scopes: ["requests:write"],
          groups: ["developers", "platform-admins"],
          roles: [{ role: "platform-admin", scope: "global", groups: ["platform-admins"] }],
          expiresAt: "2027-01-01T00:00:00Z",
          lastUsedAt: "2026-11-08T22:00:00Z",
        },
      ],
      state: "active",
      lastLogin: "2026-11-09T07:30:00Z",
      authTime: "2026-11-09T07:29:00Z",
    },
    {
      name: "carol",
      groups: ["sg-quai-shop"],
      teams: ["shop"],
      roles: [],
      tokens: [],
      state: "deactivated",
      lastLogin: "2026-10-01T07:30:00Z",
      deactivatedAt: "2026-10-02T00:00:00Z",
    },
    {
      name: "sam",
      objectId: "8f0c-oid",
      groups: ["sg-quai-platform", "sg-quai-shop"],
      teams: ["platform", "shop"],
      roles: [{ role: "platform-admin", scope: "global", groups: ["sg-quai-platform"] }],
      tokens: [],
      state: "active",
      lastLogin: "2026-11-07T10:00:00Z",
    },
  ],
})

export const access = { reads: 0 }

export const accessHandlers = (roles: () => { role: string }[] | null) => [
  http.get("*/api/v1/access-report", () => {
    const mine = roles()
    if (!mine) return HttpResponse.json({ title: "Unauthorized", status: 401, detail: "not signed in" }, { status: 401 })
    if (!mine.some((r) => r.role === "auditor" || r.role === "platform-admin"))
      return HttpResponse.json({ title: "Forbidden", status: 403, detail: "reading the access report needs the auditor role" }, { status: 403 })
    access.reads++
    return HttpResponse.json(report())
  }),
]

export function resetAccess() {
  access.reads = 0
}
