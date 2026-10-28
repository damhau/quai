import { readFileSync } from "node:fs"
import { expect, request, type APIRequestContext, type Page } from "@playwright/test"

/** signIn switches to a demo user of the development sign-in, then opens next. */
export async function signIn(page: Page, user: "alice" | "bob" | "carol" | "fiona", next = "/") {
  await page.goto(`/login?next=${encodeURIComponent(next)}`)
  await page.getByRole("button", { name: new RegExp(`^${user}`) }).click()
  await expect(page.getByRole("button", { name: "Account" })).toHaveText(user)
}

/** unique is a suffix for what a run creates, so that runs on the same stack do not collide. */
export const unique = () => Date.now().toString(36).slice(-5)

export const preview = (page: Page) => page.getByRole("region", { name: "What will be committed" })

/** approve opens a request from the approvals inbox and approves it. */
export async function approve(page: Page, user: "alice" | "fiona", title: string) {
  await signIn(page, user, "/approvals")
  await page.getByRole("link", { name: title }).click()
  await page.getByRole("button", { name: "Approve" }).click()
  await expect(page.getByText(new RegExp(`Approved by .*\\b${user}\\b`))).toBeVisible()
}

/** delivered waits for the request on screen to be committed to the repository. */
export async function delivered(page: Page) {
  await expect(page.getByText("Applied", { exact: true })).toBeVisible({ timeout: 60_000 })
}

/** gitea is the local Gitea's API, as Quai's account (the token the seed step left). */
async function gitea(): Promise<APIRequestContext> {
  const token = readFileSync(process.env.E2E_GITEA_TOKEN_FILE ?? "/run/quai/git-token", "utf8").trim()
  return request.newContext({ baseURL: process.env.E2E_GITEA_URL ?? "http://localhost:3000", extraHTTPHeaders: { Authorization: `token ${token}` } })
}

type PullRequest = { merged: boolean; state: string; head: { sha: string } }

/** pullRequest returns the number of the request's pull request, as the request page links it. */
export async function pullRequest(page: Page): Promise<number> {
  const link = page.getByRole("link", { name: /^Pull request #\d+/ })
  await expect(link).toBeVisible({ timeout: 60_000 })
  return Number(/#(\d+)/.exec((await link.textContent()) ?? "")?.[1])
}

/** onGitea reads pull request number of infra-live on Gitea, and quai/approval on its head. */
export async function onGitea(number: number): Promise<{ merged: boolean; approval?: string }> {
  const api = await gitea()
  try {
    const pr = (await (await api.get(`/api/v1/repos/quai/infra-live/pulls/${number}`)).json()) as PullRequest
    const status = (await (await api.get(`/api/v1/repos/quai/infra-live/commits/${pr.head.sha}/status`)).json()) as { statuses?: { context: string; status: string }[] }
    return { merged: pr.merged, approval: status.statuses?.find((s) => s.context === "quai/approval")?.status }
  } finally {
    await api.dispose()
  }
}

/** approvalOnGitea waits until Gitea shows quai/approval in state on the pull request. */
export async function approvalOnGitea(number: number, state: "pending" | "success") {
  await expect.poll(async () => (await onGitea(number)).approval, { timeout: 60_000 }).toBe(state)
}
