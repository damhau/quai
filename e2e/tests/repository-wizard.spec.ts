import { expect, test } from "@playwright/test"
import { signIn, unique } from "./helpers"

// Registering a repository (docs/design/01-repository-connections.md#registering-a-repository):
// alice connects Gitea's network repository with the wizard, reads its health, and disconnects
// it. The repository holds no definitions, so the catalog the other scenarios use stays as it is.
test("an admin connects a repository with the wizard, reads its health, and disconnects it", async ({ page }) => {
  const name = `network-${unique()}`
  await signIn(page, "alice", "/admin/repositories")
  await page.getByRole("link", { name: "Connect a repository" }).click()

  await page.getByRole("textbox", { name: /^Name/ }).fill(name)
  await page.getByLabel("Platform").selectOption("gitea")
  await page.getByRole("textbox", { name: /^Repository URL/ }).fill("http://gitea:3000/quai/network.git")
  await page.getByRole("textbox", { name: "Credential reference" }).fill("env:QUAI_GITEA_TOKEN")
  await page.getByRole("button", { name: /Next/ }).click()

  // The access test: every check passes, and the scan finds no definitions yet.
  await page.getByRole("button", { name: "Test access" }).click()
  await expect(page.getByRole("list", { name: "Access checks" }).getByLabel("Passed")).toHaveCount(8, { timeout: 60_000 })
  await expect(page.getByText(/No .quai\/ on the branch yet/)).toBeVisible()
  await page.getByRole("button", { name: /Next/ }).click()

  // The branch protection to set, verified on Gitea: network's main is not protected.
  await expect(page.getByRole("list", { name: "Branch protection to set" })).toContainText("require quai/approval")
  await page.getByRole("button", { name: "Verify the protection" }).click()
  await expect(page.getByRole("alert")).toContainText("branch main is not protected")
  await page.getByRole("button", { name: /Next/ }).click()

  await page.getByRole("button", { name: "Generate a run token" }).click()
  await expect(page.getByLabel("Run token", { exact: true })).toContainText("quai_run_")
  await page.getByRole("button", { name: /Next/ }).click()
  await page.getByRole("button", { name: /Next/ }).click()
  await page.getByRole("button", { name: "Connect" }).click()
  await expect(page.getByText("Connected", { exact: true })).toBeVisible({ timeout: 60_000 })

  // Its health, then it goes.
  await page.getByRole("link", { name: "Open its health" }).click()
  await expect(page.getByRole("heading", { name })).toBeVisible()
  await expect(page.getByText(/0 types and 0 blueprints/)).toBeVisible()
  await page.getByRole("link", { name: "Edit" }).click()
  await page.getByRole("button", { name: "Disconnect" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Disconnect" }).click()
  await expect(page.getByRole("table", { name: "Repositories" })).toBeVisible()
  await expect(page.getByRole("link", { name })).toHaveCount(0)
})
