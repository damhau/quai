import { readFileSync } from "node:fs"
import { expect, request, test, type APIRequestContext, type Browser, type Page } from "@playwright/test"

/** The demo users of the development sign-in (deploy/compose/quai.yaml). */
export type User = "alice" | "bob" | "carol" | "fiona" | "erin"

/** signIn switches to a demo user of the development sign-in, then opens next. */
export async function signIn(page: Page, user: User, next = "/") {
  await page.goto(`/login?next=${encodeURIComponent(next)}`)
  await page.getByRole("button", { name: new RegExp(`^${user}`) }).click()
  await expect(page.getByRole("button", { name: "Account" })).toHaveText(user)
}

/** newPage opens a page in a browser context of its own, signed in to nothing: a second person
 * beside the test's page, whose session the context does not share. Its caller closes its context. */
export async function newPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ baseURL: test.info().project.use.baseURL })
  return context.newPage()
}

/** csrf is the CSRF token of the session signed in on page, which the UI sends with each change. */
export async function csrf(page: Page): Promise<string> {
  const res = await page.request.get("/api/v1/me")
  expect(res.ok(), `GET /api/v1/me: ${res.status()}`).toBe(true)
  return ((await res.json()) as { csrfToken: string }).csrfToken
}

/** unique is a suffix for what a run creates, so that runs on the same stack do not collide. */
export const unique = () => Date.now().toString(36).slice(-5)

export const preview = (page: Page) => page.getByRole("region", { name: "What will be committed" })

/** A request on screen: its ID, its number and its title. */
export type Requested = { id: string; number: number; title: string }

/** requestNamespace has bob request a namespace on dev-1, which waits for a platform admin's
 * approval (type/platform), and leaves the page on the request. */
export async function requestNamespace(page: Page, name: string): Promise<Requested> {
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Namespace" }).click()
  await page.getByRole("radio", { name: "dev-1" }).click()
  await page.getByRole("textbox", { name: "Name" }).fill(name)
  await expect(preview(page).getByText(/type\/platform: 1 approval from/)).toBeVisible()
  await page.getByRole("button", { name: "Submit for approval" }).click()
  const title = `Create infra-live/namespace/dev-1/${name}`
  const heading = page.getByRole("heading", { name: new RegExp(title) })
  await expect(heading).toBeVisible()
  const number = Number(/#(\d+)/.exec((await heading.textContent()) ?? "")?.[1])
  expect(number, "the request's number").toBeGreaterThan(0)
  return { id: new URL(page.url()).pathname.split("/").pop() ?? "", number, title }
}

/** approve opens a request from the approvals inbox and approves it. */
export async function approve(page: Page, user: "alice" | "fiona", title: string) {
  await signIn(page, user, "/approvals")
  await page.getByRole("link", { name: title }).click()
  await page.getByRole("button", { name: "Approve" }).click()
  // Anchored: the rule's "Approved by …" line, not the card around it.
  await expect(page.getByText(new RegExp(`^Approved by .*\\b${user}\\b`))).toBeVisible()
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

/** fileOnGitea reads a file of infra-live's main branch on Gitea, as it is after the merges. */
export async function fileOnGitea(path: string): Promise<string> {
  const api = await gitea()
  try {
    const res = await api.get(`/api/v1/repos/quai/infra-live/raw/${path}?ref=main`)
    expect(res.ok(), `${path} on Gitea: ${res.status()}`).toBe(true)
    return await res.text()
  } finally {
    await api.dispose()
  }
}

/** approvalOnGitea waits until Gitea shows quai/approval in state on the pull request. */
export async function approvalOnGitea(number: number, state: "pending" | "success") {
  await expect.poll(async () => (await onGitea(number)).approval, { timeout: 60_000 }).toBe(state)
}

/** A message the local mail server (Mailpit, docker-compose.yml) caught. */
export type Message = { ID: string; Subject: string; To: { Address: string }[] }

/** mailpit is the local mail server's API, which catches what Quai sends. */
const mailpit = () => request.newContext({ baseURL: process.env.E2E_MAILPIT_URL ?? "http://localhost:8025" })

/** mailTo returns the messages to an address whose subject holds text, newest first. */
export async function mailTo(address: string, text: string): Promise<Message[]> {
  const api = await mailpit()
  try {
    const res = await api.get("/api/v1/messages?limit=1000")
    expect(res.ok(), `Mailpit: ${res.status()}`).toBe(true)
    const { messages } = (await res.json()) as { messages: Message[] }
    return messages.filter((m) => m.Subject.includes(text) && m.To.some((t) => t.Address === address))
  } finally {
    await api.dispose()
  }
}

/** mailFor waits for the one message to an address whose subject holds text, and returns it with
 * its text part. */
export async function mailFor(address: string, text: string): Promise<Message & { text: string }> {
  await expect.poll(async () => (await mailTo(address, text)).length, { message: `mail to ${address}: ${text}`, timeout: 60_000 }).toBe(1)
  const [m] = await mailTo(address, text)
  return { ...m!, text: await mailText(m!.ID) }
}

/** mailText returns the text part of a message. */
export async function mailText(id: string): Promise<string> {
  const api = await mailpit()
  try {
    return ((await (await api.get(`/api/v1/message/${id}`)).json()) as { Text: string }).Text
  } finally {
    await api.dispose()
  }
}

/** linkIn returns the path of the link to Quai a message's text ends with ("Open it in Quai: …").
 * The link names Quai's public address (QUAI_SERVER_PUBLIC_URL), localhost outside the compose
 * network; the scenarios follow its path at the address they reach Quai at. */
export function linkIn(text: string): string {
  const link = /Open it in Quai: (\S+)/.exec(text)?.[1]
  expect(link, "the message's link to Quai").toBeTruthy()
  const url = new URL(link!)
  return url.pathname + url.search
}
