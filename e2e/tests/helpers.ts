import { expect, type Page } from "@playwright/test"

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
