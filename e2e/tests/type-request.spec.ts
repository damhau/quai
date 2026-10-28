import { expect, test } from "@playwright/test"
import { approvalOnGitea, approve, delivered, onGitea, preview, pullRequest, signIn, unique } from "./helpers"

test("a VM created through the vm type lands in its environment's folder", async ({ page }) => {
  const name = `vm${unique()}`
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Virtual machine (Terraform)" }).click()
  await page.getByRole("radio", { name: "dev" }).click()
  await page.getByRole("textbox", { name: /VM name/ }).fill(name)
  await expect(preview(page).getByText(`envs/dev/vms/${name}.yaml`)).toBeVisible()
  await expect(preview(page).getByText(/No approval needed/)).toBeVisible()
  await page.getByRole("button", { name: "Submit and commit" }).click()
  await delivered(page) // no approval needed: Quai merges its pull request at once
  expect(await onGitea(await pullRequest(page))).toEqual({ merged: true, approval: "success" })

  await page.getByRole("link", { name: "Inventory" }).click()
  await page.getByRole("link", { name: `dev/${name}` }).click()
  await expect(page.getByLabel(`Content of envs/dev/vms/${name}.yaml`)).toContainText("owner: bob")
})

// The prototype's resize walkthrough: the diff touches the lines that change and keeps the
// comment a human left on them.
test("resizing a VM changes only its size, keeping the file's comments", async ({ page }) => {
  await signIn(page, "bob", "/resources/infra-live/vm/prod/web01")
  await page.getByRole("button", { name: "Resize" }).click()
  const dialog = page.getByRole("dialog")
  const cpu = dialog.getByRole("spinbutton", { name: /vCPUs/ })
  const from = Number(await cpu.inputValue())
  const to = from === 8 ? 6 : 8
  await cpu.fill(String(to))
  const diff = dialog.getByLabel("Diff of envs/prod/vms/web01.yaml")
  await expect(diff).toContainText(`+cpu: ${to}          # bumped in INC-4411`)
  await expect(diff).toContainText(`-cpu: ${from}          # bumped in INC-4411`)
  expect(await diff.locator("div").filter({ hasText: /^[-+]/ }).count()).toBe(2)
  await dialog.getByRole("button", { name: "Submit for approval" }).click()
  const title = "resize on infra-live/vm/prod/web01"
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  const pr = await pullRequest(page)
  await approvalOnGitea(pr, "pending")

  await approve(page, "alice", title)
  await delivered(page)
  expect(await onGitea(pr)).toEqual({ merged: true, approval: "success" })
  await page.goto("/resources/infra-live/vm/prod/web01")
  await expect(page.getByLabel("Content of envs/prod/vms/web01.yaml")).toContainText(`cpu: ${to}          # bumped in INC-4411`)
})
