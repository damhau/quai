import type { ConnectionView, Document, MeOutputBody, RepositoryView } from "@/api/generated/types.gen"

// Repository connections (docs/design/01-repository-connections.md): what the admin pages say
// about them.

export const platforms = [
  {
    value: "azure-devops",
    label: "Azure DevOps",
    url: "https://dev.azure.com/<organization>/<project>/_git/<repository>",
  },
  {
    value: "gitea",
    label: "Gitea",
    url: "https://git.example.com/<owner>/<repository>.git",
  },
  {
    value: "generic",
    label: "Generic Git (SSH)",
    url: "ssh://git@git.example.com/<repository>.git",
  },
] as const

export function platformLabel(connector: string): string {
  return platforms.find((p) => p.value === connector)?.label ?? connector
}

/** checkTitles name the access test's checks, in their order. */
export const checkTitles: Record<string, string> = {
  credentials: "Read the credential",
  connector: "Recognize the platform",
  read: "Read the repository",
  push: "Push a branch under the prefix",
  "pull-request": "Open and close a draft pull request",
  status: "Post a status on it",
  "delete-branch": "Delete the branch",
  "branch-protection": "Read the branch protection",
  pipelines: "Read the pipelines' runs",
}

/** isAdmin says whether the signed-in person administers repositories somewhere. */
export function isAdmin(me: MeOutputBody | undefined): boolean {
  return (me?.roles ?? []).some((r) => r.role === "platform-admin" || r.role === "repository-admin")
}

/** newDocument is the document the wizard starts from. */
export function newDocument(): Document {
  return {
    apiVersion: "quai.dev/v1beta1",
    kind: "Repository",
    metadata: { name: "", title: "" },
    spec: {
      connector: "gitea",
      url: "",
      defaultBranch: "main",
      credentials: { ref: "" },
      delivery: {
        mode: "pull-request",
        merge: "auto",
        mergeMethod: "squash",
        branchPrefix: "quai/",
      },
    },
  }
}

/** set returns d with the value at path (dot-separated) replaced. */
export function set(d: Document, path: string, value: unknown): Document {
  const next = structuredClone(d) as unknown as Record<string, unknown>
  const keys = path.split(".")
  const last = keys.pop() ?? path
  let at = next
  for (const k of keys) {
    if (typeof at[k] !== "object" || at[k] === null) at[k] = {}
    at = at[k] as Record<string, unknown>
  }
  at[last] = value
  return next as unknown as Document
}

/** pruned is the document without the empty strings the form leaves, so the server's defaults apply. */
export function pruned(d: Document): Document {
  const prune = (v: unknown): unknown => {
    if (Array.isArray(v)) return v
    if (v && typeof v === "object") {
      const out: Record<string, unknown> = {}
      for (const [k, x] of Object.entries(v)) {
        const p = prune(x)
        if (p === "" || p === undefined) continue
        if (p && typeof p === "object" && !Array.isArray(p) && Object.keys(p).length === 0) continue
        out[k] = p
      }
      return out
    }
    return v
  }
  return prune(d) as Document
}

/** protectionGuide says exactly what the target branch's protection must look like for Quai's
 * approval to be enforced, on the platform. */
export function protectionGuide(connector: string, branch: string, check: string): string[] {
  switch (connector) {
    case "azure-devops":
      return [
        `In the repository's settings, open the policies of the branch ${branch}.`,
        `Add a "Status check" policy for the status ${check} (genre ${check.split("/")[0]}, name ${check.split("/").slice(1).join("/")}).`,
        "Set its policy requirement to Required, and leave it applying by default (not only when a status is posted), without a path filter.",
        "Set its authorized identity to Quai's account, the one whose PAT is the connection's credential.",
        `Enable "Reset status whenever there are new changes".`,
      ]
    case "gitea":
      return [
        `In the repository's settings, add a branch protection rule for ${branch}.`,
        `Enable status checks and require ${check}.`,
        "Restrict pushes to the branch, so that changes go through pull requests.",
      ]
  }
  return ["Generic Git has no pull requests: delivery is direct, and approvals are enforced by Quai alone."]
}

/** Status is a connection's state at a glance. */
export type Status = { label: string; tone: "danger" | "warning" | "ok" }

export function connectionStatus(c: ConnectionView, repo?: RepositoryView): Status {
  if (!c.connected)
    return {
      label: c.error ? "Not connected" : "Connecting",
      tone: c.error ? "danger" : "warning",
    }
  if (repo?.enforcement.required && (repo.enforcement.problems?.length ?? 0) > 0) return { label: "Approvals not enforced", tone: "danger" }
  if ((repo?.trust.problems?.length ?? 0) > 0) return { label: "Definitions can change without review", tone: "warning" }
  if ((repo?.problems?.length ?? 0) > 0) return { label: "Definitions have problems", tone: "warning" }
  return { label: "Healthy", tone: "ok" }
}
